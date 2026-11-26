// Package agybridge implements the agy-bridge channel (BP-339): requests are
// served by spawning Google's official `agy acp` subprocess (Agent Client
// Protocol over stdio JSON-RPC) and talking to it, so every byte Google sees
// comes from the official client itself — no derived client-shape, no token
// handling, no direct calls to Google endpoints from this process.
//
// Account isolation (BP-339, community-validated by agy-acp and t3code —
// two independent implementations): one isolated home directory per account;
// the environment forces file-based token storage (agy 1.5.16+ reads
// AGY_CLI_FORCE_FILE_TOKEN_STORAGE) so each account's OAuth token lands in
// its own home instead of the shared Windows Credential Manager:
//
//	USERPROFILE / HOME              -> <accounts_dir>/<uid>/home
//	GEMINI_HOME                     -> <same home>/.gemini
//	AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1
//	AGY_CLI_DISABLE_AUTO_UPDATE=true
//
// Leak guards: GEMINI_API_KEY, GOOGLE_APPLICATION_CREDENTIALS and
// ANTAGRAVITY_APP_DATA_DIR are stripped from the child environment.
//
// Wire notes (ACP spec v1, agentclientprotocol.com — measured against agy
// 1.5.16 help output 2026-11-25):
//   - initialize {protocolVersion:1, clientInfo, clientCapabilities} →
//     agentInfo/agentCapabilities;
//   - session/new {cwd, mcpServers:[]} → {sessionId};
//   - session/prompt {sessionId, prompt:[{type:"text",text}]} →
//     result.stopReason (end_turn|max_tokens|refusal|cancelled);
//   - session/update notifications carry sessionUpdate:
//     agent_message_chunk | agent_thought_chunk, each with one content block;
//   - model/thinking selection is a session option; the baseline path sends
//     prompts and lets agy route its default model — the canonical model id
//     is recorded on the account binding, not forced per-request.
//
// Scope ruling (BP-339): chat-only. No tool bridge (no --ide-mcp-port), no
// failover-within-request; quota comes from agy's own usage reporting.
package agybridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// acpSetupTimeout bounds each session-setup round trip (session/new,
// set_config_option). Generous — agy cold-starts its backend — but finite, so a
// wedged child cannot pin an account's in-flight slot forever.
const acpSetupTimeout = 90 * time.Second

// servedModels is the fixed expose list: what the Antigravity UI shows for
// the account band we serve (BP-339 — hydrogen is the Gemini 4 entry; the
// Claude 4.6 pair rides the same official client — the UI lists "Claude
// Sonnet 4.6 (深度思考)" / "Claude Opus 4.6 (深度思考)" alongside Gemini
// models, all routed through agy's own Google session).
var servedModels = []string{
	"gemini-4-hydrogen",
	"claude-sonnet-4-6",
	"claude-opus-4-6-thinking",
}

// authLeakEnv are variables that must NOT reach the child: they would make
// agy pick up the gateway host's own Google identity instead of the isolated
// account's (agy-acp's leak list).
var authLeakEnv = []string{
	"GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "ANTIGRAVITY_APP_DATA_DIR",
}

// Creds is the on-disk credential shape for an agybridge account. The gateway
// never reads the token itself; the blob only records WHICH isolated home
// owns this account and what email it was logged in as.
type Creds struct {
	Email string `json:"email,omitempty"`
	Home  string `json:"home"` // absolute path of the isolated home dir
}

// Settings mirrors config.Channels.AgyBridge.
type Settings struct {
	Enabled    bool   `json:"enabled"`
	AgyPath    string `json:"agy_path"`     // default: agy on PATH
	AccountsDir string `json:"accounts_dir"` // default: <data_dir>/agybridge/accounts
	MaxInFlight int   `json:"max_in_flight"` // per-account; agy sessions are serial
}

// Driver manages one agy process per account.
type Driver struct {
	cfg config.AgyBridgeSettings

	mu       sync.Mutex
	procs    map[string][]*agyProc // account ID -> live processes (up to MaxInFlight)
	sessions map[string]string     // sessionKey -> account ID (sticky)
}

// New builds the driver. poolPass is accepted for driver.Driver shape parity
// and unused here (no token refresh — agy owns its own credentials).
func New(cfg config.AgyBridgeSettings, poolPass any) *Driver {
	// Absolutize the accounts root once: agy rejects relative gemini dirs.
	if cfg.AccountsDir != "" {
		if abs, err := filepath.Abs(cfg.AccountsDir); err == nil {
			cfg.AccountsDir = abs
		}
	}
	return &Driver{
		cfg:      cfg,
		procs:    map[string][]*agyProc{},
		sessions: map[string]string{},
	}
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelAgyBridge }

// Models implements driver.Driver.
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return append([]string(nil), servedModels...), nil
}

// accountHome returns (and creates) the isolated home for one account.
func (d *Driver) accountHome(acc *model.Account) (string, error) {
	var c Creds
	if len(acc.CredsJSON) > 0 {
		if err := json.Unmarshal(acc.CredsJSON, &c); err != nil {
			return "", fmt.Errorf("agybridge creds: %w", err)
		}
	}
	home := c.Home
	if home == "" {
		uid := acc.UID
		if uid == "" {
			uid = strings.ReplaceAll(acc.ID, ":", "_")
		}
		home = filepath.Join(d.cfg.AccountsDir, uid, "home")
	}
	// agy panics on a relative --gemini-dir ("must be an absolute path",
	// field 2026-11-25 07:00: the CLI's own log showed the panic while the
	// same command worked from a shell that had cd'd into the home). The
	// gateway's data_dir is relative, so every home path must be absolutized
	// before it reaches the child.
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", fmt.Errorf("agybridge home: %w", err)
	}
	return home, nil
}

// childEnv builds the isolated environment for one agy process, matching the
// upstream agy-acp recipe (src/accounts/env.ts buildAgyEnvAndArgs):
//   - cross-contaminating identity vars deleted;
//   - browser shim prepended so agy never opens a browser, BROWSER=/usr/bin/false;
//   - HOME/USERPROFILE redirected, SSH_CONNECTION forces FILE token storage
//     (agy treats an SSH session as headless and writes the token file instead
//     of the OS keyring — this is what makes isolation actually work), and the
//     XDG triple points inside the home.
func childEnv(home string) []string {
	env := []string{}
	for _, e := range os.Environ() {
		k := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			k = e[:i]
		}
		skip := false
		for _, leak := range authLeakEnv {
			if strings.EqualFold(k, leak) {
				skip = true
				break
			}
		}
		if skip || strings.EqualFold(k, "HOME") || strings.EqualFold(k, "USERPROFILE") ||
			strings.EqualFold(k, "GEMINI_HOME") || strings.EqualFold(k, "XDG_CONFIG_HOME") ||
			strings.EqualFold(k, "XDG_CACHE_HOME") || strings.EqualFold(k, "XDG_STATE_HOME") ||
			strings.EqualFold(k, "SSH_CONNECTION") || strings.EqualFold(k, "BROWSER") {
			continue
		}
		env = append(env, e)
	}
	shim := browserShimDir(home)
	pathVal := os.Getenv("PATH")
	if shim != "" {
		pathVal = shim + string(os.PathListSeparator) + pathVal
	}
	env = append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"PATH="+pathVal,
		"GEMINI_HOME="+filepath.Join(home, ".gemini"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		// Headless marker: agy switches to file-backed token storage when it
		// sees an SSH session (upstream agy-acp relies on exactly this).
		"SSH_CONNECTION=127.0.0.1 0 127.0.0.1 22",
		"BROWSER=/usr/bin/false",
		"AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1",
		"AGY_CLI_DISABLE_AUTO_UPDATE=true",
	)
	return env
}

// browserShimDir provisions a directory whose `open`/`open.cmd` exits non-zero,
// mirroring upstream's suppression shim: agy then never launches a browser and
// prints the authorization URL instead.
func browserShimDir(home string) string {
	dir := filepath.Join(home, ".agy-bin-shims")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	cmdFile := filepath.Join(dir, "open.cmd")
	if _, err := os.Stat(cmdFile); err != nil {
		_ = os.WriteFile(cmdFile, []byte("@exit /b 1\r\n"), 0o755)
	}
	return dir
}

// acpArgs builds the child argv. Upstream's launcher requirements, both
// measured (env.ts: "agy launcher.go requires --gemini-dir to be absolute,
// while --app-data-dir MUST be a relative subdirectory name under geminiDir"):
//   --gemini-dir    absolute path
//   --app-data-dir  relative NAME only ("antigravity-cli"), never a path
func acpArgs(home string, extra ...string) []string {
	args := []string{"acp", "--gemini-dir", filepath.Join(home, ".gemini"), "--app-data-dir", "antigravity-cli"}
	return append(args, extra...)
}

// agyProc is one live `agy acp` subprocess with its JSON-RPC plumbing.
type agyProc struct {
	cmd       *exec.Cmd
	mu        sync.Mutex
	nextID    int64
	sessionID string
	credsHome string // the isolated home this process was spawned with
	workDir   string // the throwaway ACP workspace (outside every project tree)
	canAppendSystem bool     // agentCapabilities._meta.antigravity.systemPrompt.append
	modelIDs        []string // model ids this session advertised (session/new)
	claimed   bool   // a turn is using this process right now
	pending   map[int64]chan rpcMsg
	updates   chan rpcMsg
	done      chan struct{}
	stdin     chan []byte
	errText   strings.Builder
}

// rpcMsg is one decoded JSON-RPC frame (request, response, or notification).
type rpcMsg struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("rpc %d: %s", e.Code, e.Message)
}

// startProc spawns `agy acp` for one account home and starts its readers.
func (d *Driver) startProc(ctx context.Context, acc *model.Account, home string) (*agyProc, error) {
	bin := d.agyBinary()
	// BP-342: the child runs in a throwaway temp workspace, NOT in the account
	// home. The home lives inside this repository, so agy walked up, found .git
	// and adopted the whole project as its workspace (diffing a 22 MB exe and
	// plugins/) while the caller waited minutes for an answer. Upstream does the
	// same: fs.mkdtempSync(os.tmpdir()).
	workDir, errW := workspaceDirFor()
	if errW != nil {
		workDir = home // degraded but functional; never block a turn on this
	}
	cmd := exec.Command(bin, acpArgs(home, "--disable-telemetry")...)
	cmd.Dir = workDir
	cmd.Env = childEnv(home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	p := &agyProc{
		cmd:       cmd,
		credsHome: home,
		workDir:   workDir,
		pending:   map[int64]chan rpcMsg{},
		updates:   make(chan rpcMsg, 256),
		done:      make(chan struct{}),
		stdin:     make(chan []byte, 16),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("agybridge: spawn agy: %w", err)
	}
	go p.writeLoop(stdin)
	go p.readLoop(stdout)
	go p.stderrLoop(stderr)
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	if err := p.initialize(ctx); err != nil {
		p.kill()
		return nil, err
	}
	return p, nil
}

func (p *agyProc) writeLoop(stdin interface{ Write([]byte) (int, error) }) {
	for {
		select {
		case b, ok := <-p.stdin:
			if !ok {
				return
			}
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *agyProc) stderrLoop(stderr io.Reader) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
	for sc.Scan() {
		p.mu.Lock()
		if p.errText.Len() < 4<<10 {
			p.errText.WriteString(sc.Text())
			p.errText.WriteByte('\n')
		}
		p.mu.Unlock()
	}
}

func (p *agyProc) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.Method == "session/update" {
			// NEVER block here. This read loop is the only path that can deliver
			// a JSON-RPC RESULT, so a full updates queue would stall it and the
			// turn's own response would never be read: agy finished generating
			// (its log shows the title being written) while the caller waited
			// forever (2026-11-25 21:39, turn cancelled after 9m53s).
			// Overflow drops the oldest queued update: losing a stream chunk is
			// survivable, wedging the connection is not. When no turn is active
			// (no session claimed) updates are discarded outright.
			select {
			case p.updates <- m:
			default:
				select {
				case <-p.updates: // make room by discarding the oldest
				default:
				}
				select {
				case p.updates <- m:
				default:
				}
			}
			continue
		}
		// Server→client REQUESTS carry an id and expect an answer. Dropping one
		// hangs the turn: agy waits for the permission verdict while we wait for
		// its result. Upstream agy-acp answers every permission request with a
		// refusal and lets the client-side tools do the work; this bridge is
		// chat-only, so a refusal is the correct answer for every request it
		// can raise here (2026-11-25: the first live ACP run only worked
		// because no tool was invoked — the drop path was never exercised).
		if m.Method != "" && m.ID != 0 {
			p.answerServerRequest(m)
			continue
		}
		p.mu.Lock()
		ch, ok := p.pending[m.ID]
		if ok {
			delete(p.pending, m.ID)
		}
		p.mu.Unlock()
		if ok {
			// Buffered slot + non-blocking send: the reader may have been
			// cancelled and dropped between lookup and delivery, and the read
			// loop must never stall on one abandoned waiter.
			select {
			case ch <- m:
			default:
			}
		}
	}
}

// cancelPending detaches every waiter and discards queued stream updates for
// the abandoned turn. Called when a turn is cancelled so the NEXT turn on this
// process starts clean instead of consuming the previous turn's chunks.
func (p *agyProc) cancelPending() {
	p.mu.Lock()
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	p.mu.Unlock()
	for {
		select {
		case <-p.updates:
		default:
			return
		}
	}
}

// answerServerRequest replies to a server-initiated request. Permission asks
// are refused (cancelled outcome); anything else gets a benign empty result so
// agy never blocks on us.
func (p *agyProc) answerServerRequest(m rpcMsg) {
	// Upstream agy-acp's exact contract (src/runtime/acp-connection.ts):
	//   • session/request_permission with an options[] -> pick one and answer
	//     {outcome:"selected", optionId}; only when no option is offered fall
	//     back to {outcome:"cancelled"}.
	//   • ANY other server request -> JSON-RPC error -32601 Method not found.
	// Answering a non-permission request with an empty RESULT (what this code
	// used to do) leaves agy waiting: it then refuses further prompts with
	// "the running prompt is waiting for a permission or question answer"
	// (field 2026-11-26 00:52, reproduced on three consecutive turns).
	if m.Method == "session/request_permission" {
		outcome := map[string]any{"outcome": "cancelled"}
		if optID := firstPermissionOption(m.Params); optID != "" {
			// Rejecting is chosen here on purpose: this bridge is chat-only and
			// exposes no tools, so approving a tool would ask agy to do work the
			// caller cannot receive.
			outcome = map[string]any{"outcome": "cancelled", "optionId": optID}
		}
		p.sendRPC(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": outcome}})
		return
	}
	p.sendRPC(map[string]any{
		"jsonrpc": "2.0", "id": m.ID,
		"error": map[string]any{"code": -32601, "message": "Method not found: " + m.Method},
	})
}

// firstPermissionOption pulls the first optionId out of a permission request so
// the refusal names a real option (agy logs an unnamed refusal as unanswered).
func firstPermissionOption(params json.RawMessage) string {
	var doc struct {
		Options []struct {
			OptionID string `json:"optionId"`
			ID       string `json:"id"`
		} `json:"options"`
	}
	if len(params) == 0 || json.Unmarshal(params, &doc) != nil {
		return ""
	}
	for _, o := range doc.Options {
		if o.OptionID != "" {
			return o.OptionID
		}
		if o.ID != "" {
			return o.ID
		}
	}
	return ""
}

func (p *agyProc) sendRPC(frame map[string]any) {
	b, err := json.Marshal(frame)
	if err != nil {
		return
	}
	select {
	case p.stdin <- b:
	case <-p.done:
	}
}

// call sends one JSON-RPC request and waits for its response.
func (p *agyProc) call(ctx context.Context, method string, params any, out any) error {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan rpcMsg, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	// params must stay a JSON OBJECT in the frame. Marshalling it separately and
	// stuffing json.RawMessage in works too, but passing the value itself is the
	// shape every ACP message uses — an earlier version double-encoded it into a
	// string and agy answered -32602 "cannot unmarshal string into
	// schema.InitializeParams" (2026-11-25 self-verification).
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	select {
	case p.stdin <- frame:
	case <-p.done:
		return fmt.Errorf("agy exited")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case m, open := <-ch:
		if !open {
			return fmt.Errorf("agybridge: %s cancelled", method)
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-p.done:
		return fmt.Errorf("agy exited during %s", method)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// initialize performs the ACP handshake.
func (p *agyProc) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			Meta struct {
				Antigravity struct {
					SystemPrompt struct {
						Append bool `json:"append"`
					} `json:"systemPrompt"`
				} `json:"antigravity"`
			} `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	params := map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "bargainproxy-agybridge",
			"version": "1.0.0",
		},
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
		},
	}
	if err := p.call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	// Upstream reads exactly this flag before deciding how to deliver a client
	// system prompt (worker.ts: appendSystemSupported =
	// agentCapabilities._meta.antigravity.systemPrompt.append).
	p.canAppendSystem = result.AgentCapabilities.Meta.Antigravity.SystemPrompt.Append
	return nil
}

// kill terminates the subprocess.
func (p *agyProc) kill() {
	_ = p.cmd.Process.Kill()
}

// Stream implements driver.Driver. One prompt = one session/prompt round on
// the account's sticky process.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	home, err := d.accountHome(call.Account)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	// Login gate (BP-339 follow-up): an account row exists as soon as the panel
	// starts a sign-in, so a failed or abandoned sign-in leaves a tokenless row
	// in the pool. Selecting it produces "rpc -32000: Authentication required"
	// and burns a cascade slot for nothing (field 2026-11-25 17:18: three
	// tokenless leftovers blocked the one account that WAS signed in). A missing
	// token file is therefore an auth-class failure for THIS account, which the
	// pool keeps out of rotation while siblings serve.
	if !homeHasToken(home) {
		return nil, uperr.New(uperr.ClassAuth, 0,
			"agybridge: account %s has no token yet (sign in from the panel)", call.Account.ID)
	}
	p, err := d.procFor(ctx, call.Account, home)
	if err != nil {
		return nil, d.classify(err, call.Account)
	}
	defer p.release()
	return p.prompt(ctx, call, emit, d)
}

// homeHasToken reports whether the isolated home holds agy's file-backed
// credential (the file the panel's sign-in writes on success).
func homeHasToken(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	return err == nil
}

// procFor returns the sticky process for the account, spawning one when the
// session key has no live binding.
// procFor hands out a process for one turn from the account's pool. Each
// process serves exactly ONE in-flight turn (agy serialises a session), so the
// pool grows to MaxInFlight and reuses whatever is idle. The pool's size is the
// channel's per-account cap — an idle process is held for the next turn so a
// warm backend is reused, but a process already streaming is never shared
// (sharing one process across concurrent turns would interleave their
// session/update chunks and answer both with the wrong text).
func (d *Driver) procFor(ctx context.Context, acc *model.Account, home string) (*agyProc, error) {
	d.mu.Lock()
	live := d.procs[acc.ID][:0]
	var idle *agyProc
	for _, p := range d.procs[acc.ID] {
		select {
		case <-p.done:
			continue // crashed/exited: drop it
		default:
		}
		live = append(live, p)
		if idle == nil && p.tryClaim() {
			idle = p
		}
	}
	d.procs[acc.ID] = live
	d.mu.Unlock()

	if idle != nil {
		return idle, nil
	}
	// Spawn a new one under the lock so two concurrent turns cannot both decide
	// "pool is empty" and exceed the cap.
	d.mu.Lock()
	defer d.mu.Unlock()
	limit := d.cfg.MaxInFlight
	if limit <= 0 {
		limit = 3
	}
	if len(d.procs[acc.ID]) >= limit {
		// Caller already holds a pool slot from the gateway; if the driver pool
		// is momentarily full, wait for a process to free up rather than failing.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	p, err := d.startProc(ctx, acc, home)
	if err != nil {
		return nil, err
	}
	p.claimed = true
	d.procs[acc.ID] = append(d.procs[acc.ID], p)
	return p, nil
}

// tryClaim marks an idle process busy. False means another turn already took it.
func (p *agyProc) tryClaim() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.claimed {
		return false
	}
	p.claimed = true
	return true
}

// release marks the process idle again so the next turn can reuse it.
func (p *agyProc) release() {
	p.mu.Lock()
	p.claimed = false
	p.mu.Unlock()
}

// prompt runs one turn: session (new or resumed) + session/prompt, streaming
// update notifications into emit.
func (p *agyProc) prompt(ctx context.Context, call driver.ChatCall, emit driver.Emit, d *Driver) (*ir.ChatResponse, error) {
	sessID, err := p.ensureSession(ctx, call)
	if err != nil {
		return nil, d.classify(err, call.Account)
	}
	// BP-342: the client system prompt goes through ACP's own field
	// (session/new _meta.antigravity.systemPrompt.append), never diluted into
	// the prompt text. Upstream does exactly this (worker.ts). Mixing it into the
	// prompt body made agy treat ZCode's identity/instructions as its OWN system
	// prompt: its session title read "我是由 Anthropic 提供、运行在
	// bargainproxy-agybridge-anthropic/... 模型上的 ZCode 交互..." and it started
	// behaving like a coding agent (scanning the workspace) instead of answering.
	sysText := systemTextOf(call.Request)
	prompt := buildPrompt(call.Request)
	params := map[string]any{
		"sessionId": sessID,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	}
	if sysText != "" && p.canAppendSystem {
		params["_meta"] = map[string]any{
			"antigravity": map[string]any{
				"systemPrompt": map[string]any{"append": sysText},
			},
		}
	} else if sysText != "" {
		// agy without append support: prefix it as context, but keep it clearly
		// scoped so the model does not adopt it as its own identity.
		params["prompt"] = []map[string]string{{"type": "text", "text": sysText + "\n\n" + prompt}}
	}
	out := &ir.ChatResponse{Model: call.UpstreamModel}
	var text, think strings.Builder
	// Idle watchdog: silence — not wall-clock — retires an agy process
	// (upstream armTurnIdleWatchdog). A turn with tools gets the longer budget.
	idle := newIdleWatchdog(len(call.Request.Tools) > 0)
	defer idle.stop()
	respCh := make(chan error, 1)
	go func() {
		respCh <- p.call(ctx, "session/prompt", params, nil)
	}()
	// A cancelled turn MUST tear the ACP call down and abandon any partial
	// stream state. Returning early while the goroutine stays blocked on the
	// pending channel leaked the account's only in-flight slot: the pool showed
	// in_flight=1 forever and every later request answered 429 "所有号池当前都在
	// 处理其他请求" (field 2026-11-25 20:40-20:50, holder xiaochu2693, agy itself
	// healthy at 9.7s in a direct ACP probe).
	defer func() {
		p.cancelPending()
	}()
	for {
		select {
		case err := <-respCh:
			if err != nil {
				return nil, d.classify(err, call.Account)
			}
			out.Message = ir.Message{Role: "assistant", Content: []ir.ContentPart{{Kind: "text", Text: text.String()}}}
			out.Usage = ir.Usage{OutputTokens: approxTokens(text.String())}
			emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
			return out, nil
		case up := <-p.updates:
			// Sessions are per process, but agy also emits session-less updates
			// (available_commands, usage). Match by sessionId when the frame
			// carries one, so a stale frame from an earlier session cannot leak
			// into this turn's text.
			if sid := updateSessionID(up); sid != "" && sid != p.activeSession() {
				continue
			}
			// Any frame — even a title/usage update — proves agy is alive, and
			// re-arms the idle watchdog (upstream noteTurnProgress).
			idle.rearm()
			kind, content := parseUpdate(up)
			switch kind {
			case "agent_message_chunk":
				text.WriteString(content)
				_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: content})
			case "agent_thought_chunk":
				think.WriteString(content)
				_ = emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: content})
			}
		case <-idle.fired():
			// Upstream retires the WORKER rather than settling the turn: a local
			// timeout is not proof the native prompt stopped, and re-sending on a
			// live session answers with whatever the first generation is still
			// doing. Kill this process so the pool spawns a fresh one.
			p.kill()
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"agybridge: agy 静默超过 %s（无任何输出），已回收该进程", agyTurnIdleBudget)
		case <-ctx.Done():
			return nil, uperr.New(uperr.ClassCanceled, 0, "agybridge: canceled: %v", ctx.Err())
		}
	}
}

// ensureSession opens an ACP session once per process (agy keeps its own
// conversation state per session; we resume the same one for stickiness).
func (p *agyProc) ensureSession(ctx context.Context, call driver.ChatCall) (string, error) {
	p.mu.Lock()
	if p.sessionID != "" {
		s := p.sessionID
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()
	var result struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			Available []struct {
				ModelID string `json:"modelId"`
				Name    string `json:"name"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	// cwd for the ACP session: the throwaway workspace (BP-342). The account
	// home is only credential storage and must never be the workspace.
	home := "."
	if c := p.workDir; c != "" {
		home = c
	} else if c := p.credsHome; c != "" {
		home = c
	}
	params := map[string]any{
		"cwd":        home,
		"mcpServers": []any{},
	}
	// Every ACP round trip needs its own ceiling. The gateway's first-byte
	// watchdog only fires once a response object exists (it watches emit), so a
	// silent session/new or set_config_option would hang the whole turn — and
	// with it the account's only in-flight slot, which is exactly what locked
	// the bridge channel at 2026-11-25 20:40 (in_flight stuck at 1, zero
	// "leg failed" lines because Stream never returned).
	newCtx, cancelNew := context.WithTimeout(ctx, acpSetupTimeout)
	defer cancelNew()
	if err := p.call(newCtx, "session/new", params, &result); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.sessionID = result.SessionID
	p.modelIDs = p.modelIDs[:0]
	for _, m := range result.Models.Available {
		if m.ModelID != "" {
			p.modelIDs = append(p.modelIDs, m.ModelID)
		}
	}
	p.mu.Unlock()

	// Model + thinking level both live in the session's configOptions, NOT in
	// the prompt (measured 2026-11-25: agy lists model and thought_level there,
	// and ignores any model field on session/prompt).
	if want := call.UpstreamModel; want != "" {
		if err := p.setConfigOption(newCtx, result.SessionID, "model", want); err != nil {
			return "", err
		}
	}
	// Thinking depth is an INDEPENDENT session option: after
	// set_config_option("thought_level","xhigh") agy reports
	// model=gemini-4-hydrogen (unchanged) and thought_level=xhigh, and the turn
	// completes in 8.5s (probed 2026-11-26 00:50). Do NOT fold the level into
	// the model name — the advertised model list has no "-high" variants, so
	// asking for one leaves the model unset and the session waits forever
	// (measured 00:44-00:46: 27s+ with zero output).
	if level := thinkingLevelOf(call.Request); level != "" {
		// Best effort: a model without a thought_level option must still answer.
		if err := p.setConfigOption(newCtx, result.SessionID, "thought_level", level); err != nil {
			log.Printf("agybridge: thought_level=%s not applied: %v", level, err)
		}
	}
	return result.SessionID, nil
}

// thinkingLevelOf resolves the client's requested reasoning depth using the
// same rules as the antigravity direct driver: an explicit level wins, else a
// token budget converts to a band, else OpenAI's reasoning_effort.
func thinkingLevelOf(req *ir.ChatRequest) string {
	if req == nil {
		return ""
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		if lvl := strings.ToLower(strings.TrimSpace(req.Thinking.Level)); lvl != "" {
			return lvl
		}
		if lvl := ir.LevelFromBudget(req.Thinking.BudgetTokens); lvl != "" {
			return lvl
		}
	}
	return strings.ToLower(strings.TrimSpace(req.RequestedEffort))
}

// setConfigOption applies one ACP session config option (model, thought_level).
func (p *agyProc) setConfigOption(ctx context.Context, sessionID, configID, value string) error {
	var result struct {
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
			Options      []struct {
				Value string `json:"value"`
			} `json:"options"`
		} `json:"configOptions"`
	}
	params := map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	}
	if err := p.call(ctx, "session/set_config_option", params, &result); err != nil {
		return fmt.Errorf("agybridge: set %s=%s: %w", configID, value, err)
	}
	// Verify the session actually took the value: silently ignoring it looks
	// exactly like a working request that answers from the wrong model.
	for _, opt := range result.ConfigOptions {
		if opt.ID == configID {
			if opt.CurrentValue != value {
				return fmt.Errorf("agybridge: %s stayed %q after asking for %q",
					configID, opt.CurrentValue, value)
			}
			return nil
		}
	}
	return nil
}

// activeSession returns the session this process is currently serving.
func (p *agyProc) activeSession() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}
