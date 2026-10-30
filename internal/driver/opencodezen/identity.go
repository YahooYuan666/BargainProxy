// Package opencodezen implements the OpenCode Zen anonymous free-model
// channel: OpenAI chat-completions wire upstream with a hard free-tier gate —
// requests must stream and must carry "bash"+"read" function tools, plus a
// full CLI disguise header set — or the upstream answers 403 FreeTierError.
//
// Behaviour constants and rules are clean-room ported from the MIT-licensed
// opencode2zcode project (rust/src/zen.rs, https://github.com/YahooYuan666/
// opencode2zcode, MIT © 2026; see NOTICE). The disguise material (client
// version, header names, session-id shape) is what the official OpenCode CLI
// itself sends; it is not a secret and lives in one place so an upstream
// tightening is a one-constant change.
package opencodezen

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Anonymous free-lane bearer: a fixed public placeholder, not a credential.
const AnonymousKey = "public"

// openCodeClientVersion is the CLI version the disguise mimics. Upstream
// tightens against old versions; bump here when it does (2026-10-18: the
// free lane started answering 400 FreeTierError to 1.18.31).
//
// 2.0.25 is what magpie-community/plugins packages/zen-free captured from
// OpenCode's own requests (2026-10-25). 2026-10-26 Zen's free tier started
// answering 429 FreeUsageLimitError ("Free models work in OpenCode") to the
// exact UA the 1.19.0 disguise sent — while the same request under ANY other
// string, 2.0.25's included, is served — and 2.x unified the UA across
// protocols to `opencode/<channel>/<version>/<name>`, so the per-protocol
// ai-sdk provider-utils segments the 1.x UA carried are history (BP-97 kept
// them accurate for 1.19.0; BP-127 follows the 2.x format).
// openCodeClientVersion is the OpenCode release the free lane currently
// trusts. The desktop 2.2.2 is what the user runs and what the upstream sees
// succeed (BP-219 capture, 2026-10-30); its UA is
// `opencode/latest/2.2.2/desktop` and its client header is `desktop`
// (packages/desktop/src/main/lifecycle/environment.ts sets OPENCODE_CLIENT).
const openCodeClientVersion = "2.2.2"

// gateToolNames are the function tools the free lane requires on every
// request. OpenCode 2.2.x renamed its shell tool (BP-219 capture: the desktop
// offers shell/edit/glob/grep/read with REAL parameter schemas — the earlier
// one-property stand-ins and the 1.x "bash" name belong to 1.19-era gates).
var gateToolNames = []string{"shell", "edit", "glob", "grep", "read"}

// acceptedEfforts are the reasoning_effort values the upstream actually
// accepts; anything else (off/none/unknown) must be omitted, not passed
// through — passing "none" is a 400.
var acceptedEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

const gateDescription = "Reserved for the host runtime; do not call it."

// --- OpenCode environment block ---------------------------------------------

// zenEnvironmentFor renders OpenCode's OWN environment block. The upstream
// gate is not "a block exists" but "this looks like the block OpenCode
// writes": magpie-community/plugins packages/zen-free provider.mjs says a
// fixed text only this plugin sends is itself the fingerprint (that is how
// "Working directory: unknown" got refused on 2026-10-29, and how the 1.19.0
// User-Agent got refused before it). So the block is OpenCode 2.2.0's own,
// six lines, byte-shaped as its packages/core builtins.ts writes them:
//
//	Here is some useful information about the environment you are running in:
//	<env>
//	  Working directory: <dir>
//	  Workspace root folder: <dir>
//	  Is directory a git repo: yes|no
//	  Platform: win32|darwin|linux
//	</env>
//
// The directory is the request's own when its prompt names one, else this
// process's working directory, else the user's home. "unknown" is never
// sent — that literal is exactly what the gate punishes.
func zenEnvironmentFor(body map[string]any) string {
	dir := extractWorkingDir(body)
	if dir == "" {
		if wd, err := os.Getwd(); err == nil && wd != "" {
			dir = wd
		}
	}
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dir = home
		}
	}
	if dir == "" {
		dir = "." // nothing derivable: the current-directory shorthand
	}
	return "Here is some useful information about the environment you are running in:\n" +
		"<env>\n" +
		"  Working directory: " + dir + "\n" +
		"  Workspace root folder: " + dir + "\n" +
		"  Is directory a git repo: " + zenGitFlag(dir) + "\n" +
		"  Platform: " + zenPlatform() + "\n" +
		"</env>"
}

// zenPlatform is the platform name OpenCode writes: node's process.platform
// spelling (win32/darwin/linux), NOT Go's GOOS (windows/darwin/linux) — the
// difference on Windows alone would be a fingerprint.
func zenPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

// zenGitFlag answers "Is directory a git repo" the way OpenCode does: a
// .git entry in the directory itself. A directory we cannot stat says "no"
// (the same thing OpenCode's own try/catch leaves behind).
func zenGitFlag(dir string) string {
	if dir == "" || dir == "." {
		return "no"
	}
	expanded := dir
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(dir, "~/"); ok {
			expanded = filepath.Join(home, rest)
		}
	}
	if _, err := os.Stat(filepath.Join(expanded, ".git")); err == nil {
		return "yes"
	}
	return "no"
}

var (
	zenCwdRe         = regexp.MustCompile(`<cwd>([^<\n]{1,300})</cwd>`)
	zenWorkDirLineRe = regexp.MustCompile(`(?m)^\s*Working directory:[ \t]*(\S[^\n]{0,300})`)
)

// extractWorkingDir digs a working directory out of the request's own texts:
// OpenCode embeds `<cwd>…</cwd>` in its env block, other harnesses write a
// "Working directory: …" line. The literal "unknown" is skipped — it is what
// the upstream now punishes.
func extractWorkingDir(body map[string]any) string {
	texts := zenScanTexts(body)
	for _, re := range []*regexp.Regexp{zenCwdRe, zenWorkDirLineRe} {
		for _, s := range texts {
			if m := re.FindStringSubmatch(s); m != nil {
				if dir := strings.TrimSpace(m[1]); dir != "" && !strings.EqualFold(dir, "unknown") {
					return dir
				}
			}
		}
	}
	return ""
}

// zenScanTexts collects the request's own text values, system messages (and
// the Responses instructions) first: that is where a real client's env block
// lives. Nothing is copied — the strings are borrowed for matching only.
func zenScanTexts(body map[string]any) []string {
	var system, rest []string
	if s, ok := body["instructions"].(string); ok && s != "" {
		system = append(system, s)
	}
	switch msgs := body["messages"].(type) {
	case []map[string]any:
		for _, mm := range msgs {
			role, _ := mm["role"].(string)
			if role == "system" {
				appendTextInto(&system, mm["content"])
			} else {
				appendTextInto(&rest, mm["content"])
			}
		}
	case []any:
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			if role == "system" {
				appendTextInto(&system, mm["content"])
			} else {
				appendTextInto(&rest, mm["content"])
			}
		}
	}
	return append(system, rest...)
}

func appendTextInto(dst *[]string, content any) {
	switch c := content.(type) {
	case string:
		if c != "" {
			*dst = append(*dst, c)
		}
	case []any:
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if txt, _ := pm["text"].(string); txt != "" {
					*dst = append(*dst, txt)
				}
			}
		}
	}
}

var zenHasEnvironment = regexp.MustCompile(`(?s)<env>.*Working directory:.*</env>`)

// harnessSystem is OpenCode's own agent preamble, captured from a live 2.2.2
// desktop request (BP-219, 2026-10-30) and embedded verbatim. The free-lane
// gate reads the system text's SHAPE: a request whose first message is not
// OpenCode's own harness answers 429 FreeUsageLimitError no matter what the
// headers say (live matrix: harness+tools+env 200; any one of them missing
// 429; the one-line "You are opencode" stand-in 429 too). The embedded text
// is the static part only — it stops before the user-specific MCP
// instructions, references, skills and AGENTS.md the desktop appends.
//
//go:embed assets/harness_system.md
var harnessSystem string

// zenWorktreeNote is the line the desktop appends after the env block.
const zenWorktreeNote = "When you create a worktree outside the current working directory and intend to use it as your primary working directory, consider using `execute` to call `tools.opencode.session_move` and make the worktree the session's working directory."

// zenSystemTail builds what the desktop appends AFTER the user's own
// instructions: today's date, the env block, the worktree note (BP-219
// capture order). The env block rides INSIDE the system message — a top-level
// `system` field is not a chat-completions field and the gate never sees it.
func zenSystemTail(body map[string]any, withEnv bool) string {
	var b strings.Builder
	b.WriteString("Today's date: " + time.Now().Format("Mon Jan 2 2006") + "\n\n")
	if withEnv {
		b.WriteString(zenEnvironmentFor(body) + "\n")
	}
	b.WriteString(zenWorktreeNote)
	return b.String()
}

// injectEnvironment composes the first system message the way the desktop
// does: OpenCode's harness first, then the request's own system text, then
// the date/env/worktree tail (BP-219 capture). A request with no system
// message gets one. The responses lane keeps its instructions-based shape.
func injectEnvironment(body map[string]any, responsesLane bool) {
	if responsesLane {
		env := zenEnvironmentFor(body)
		s, _ := body["instructions"].(string)
		if zenHasEnvironment.MatchString(s) {
			return
		}
		body["instructions"] = withEnvironment(s, env)
		return
	}
	tail := zenSystemTail(body, true)
	// The chat build stores messages as []map[string]any (a concrete []any
	// here once silently replaced every real message — the ensureGateTools
	// lesson), so both shapes are handled in place.
	switch msgs := body["messages"].(type) {
	case []map[string]any:
		for _, mm := range msgs {
			if role, _ := mm["role"].(string); role != "system" {
				continue
			}
			mm["content"] = composeZenSystem(contentString(mm["content"]), tail)
			return
		}
		body["messages"] = append([]map[string]any{{"role": "system", "content": harnessSystem + "\n" + tail}}, msgs...)
	case []any:
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := mm["role"].(string); role != "system" {
				continue
			}
			mm["content"] = composeZenSystem(contentString(mm["content"]), tail)
			return
		}
		body["messages"] = append([]any{map[string]any{"role": "system", "content": harnessSystem + "\n" + tail}}, msgs...)
	default:
		body["messages"] = []any{map[string]any{"role": "system", "content": harnessSystem + "\n" + tail}}
	}
}

// contentString flattens the system-content shapes a caller may send (string,
// text-part array, nil) to one string for composition.
func contentString(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// composeZenSystem lays out harness + the caller's own system text + the
// date/env tail, skipping blank pieces. The caller's env, if it sent one
// already, is left inside its text and the tail omits its duplicate.
func composeZenSystem(own, tail string) string {
	withEnv := !zenHasEnvironment.MatchString(own)
	if !withEnv {
		tail = zenSystemTail(nil, false)
	}
	pieces := make([]string, 0, 3)
	if strings.TrimSpace(harnessSystem) != "" {
		pieces = append(pieces, harnessSystem)
	}
	if strings.TrimSpace(own) != "" {
		pieces = append(pieces, own)
	}
	pieces = append(pieces, tail)
	return strings.Join(pieces, "\n")
}

// withEnvironment appends the block to a system text the way the reference
// implementation does: `content\n\n<env>…`, or just the block when empty.
func withEnvironment(content, env string) string {
	if strings.TrimSpace(content) == "" {
		return env
	}
	return content + "\n\n" + env
}

// --- identity: canonical session/request/project ids ---

// invertedTimestampHex mimics the official Identifier.descending(): the
// 48-bit complement of (now_ms * 0x1000 + 1), 12 hex chars. With counter=1
// the low bits always invert to a "…ffe" tail, which the upstream validates
// (check added live 2026-10-13).
func invertedTimestampHex(nowMillis uint64) string {
	current := uint64(nowMillis)*0x1000 + 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return fmt.Sprintf("%012x", inverted)
}

func isDescendingTimePart(part string) bool {
	if len(part) != 12 {
		return false
	}
	for i := 0; i < 12; i++ {
		c := part[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return part[9] == 'f' && part[10] >= 'c' && part[10] <= 'f'
}

// isCanonicalSession reports whether value already has the official shape
// "ses_" + 12 hex + 14 base62 — such values pass through untouched.
func isCanonicalSession(value string) bool {
	rest, ok := strings.CutPrefix(value, "ses_")
	if !ok || len(rest) != 26 {
		return false
	}
	if !isDescendingTimePart(rest[:12]) {
		return false
	}
	for i := 12; i < 26; i++ {
		c := rest[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62Fixed(value uint64, width int) string {
	out := []byte(strings.Repeat("0", width))
	for i := width - 1; i >= 0; i-- {
		out[i] = base62Alphabet[value%62]
		value /= 62
	}
	return string(out)
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// sessionMemo keeps the session id stable across the retries/multi-turns of
// one conversation despite the timestamp inside its shape: keyed by the
// conversation seed, capped, like the reference implementation.
var (
	sessionMemoMu sync.Mutex
	sessionMemo   = map[string]string{}
	sessionMemoQ  []string
)

const sessionMemoLimit = 1024

// canonicalSessionID returns the canonical session id for a conversation
// seed, minting (and memoizing) one when needed.
func canonicalSessionID(seed string) string {
	if isCanonicalSession(seed) {
		return seed
	}
	sessionMemoMu.Lock()
	defer sessionMemoMu.Unlock()
	if v, ok := sessionMemo[seed]; ok {
		return v
	}
	now := uint64(time.Now().UnixMilli())
	timePart := invertedTimestampHex(now)
	tail, _ := strconvParseUint64(sha256Hex("ses\x00" + seed)[12:32])
	session := "ses_" + timePart + base62Fixed(tail, 14)
	sessionMemo[seed] = session
	sessionMemoQ = append(sessionMemoQ, seed)
	if len(sessionMemoQ) > sessionMemoLimit {
		delete(sessionMemo, sessionMemoQ[0])
		sessionMemoQ = sessionMemoQ[1:]
	}
	return session
}

func strconvParseUint64(s string) (uint64, error) {
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		default:
			return v, fmt.Errorf("bad hex")
		}
		v = v*16 + d
	}
	return v, nil
}

// mintRequestID produces the official request-id shape:
// "msg_" + 12 ascending-hex timestamp + 14 base62 random.
func mintRequestID() string {
	current := uint64(time.Now().UnixMilli())*0x1000 + 1
	timePart := fmt.Sprintf("%012x", current&0xFFFFFFFFFFFF)
	var randBytes [8]byte
	_, _ = rand.Read(randBytes[:])
	randVal := uint64(randBytes[0])<<56 | uint64(randBytes[1])<<48 |
		uint64(randBytes[2])<<40 | uint64(randBytes[3])<<32 |
		uint64(randBytes[4])<<24 | uint64(randBytes[5])<<16 |
		uint64(randBytes[6])<<8 | uint64(randBytes[7])
	return "msg_" + timePart + base62Fixed(randVal, 14)
}

// projectID is a stable per-installation project marker (not a secret).
func projectID() string {
	return "prj_" + sha256Hex("prj\x00bargainproxy:default-project")[:24]
}

// requestIDs bundles the correlation ids derived from one request.
type requestIDs struct {
	session string
	request string
	project string
}

// conversationSeed is the first user message's JSON-encoded content — the
// same conversation always derives the same session id (quota/cache affinity
// upstream). Falls back to randomness when nothing usable is present.
func conversationSeed(body []byte) string {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	pick := func(content json.RawMessage) string {
		if len(content) == 0 || string(content) == "null" {
			return ""
		}
		// Compact to keep the seed stable across insignificant whitespace.
		var v any
		if err := json.Unmarshal(content, &v); err != nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
	for _, m := range doc.Messages {
		if m.Role != "user" {
			continue
		}
		if seed := pick(m.Content); seed != "" && seed != "null" {
			return seed
		}
	}
	// Responses wire: input may be a plain string.
	if len(doc.Input) > 0 {
		var sv string
		if err := json.Unmarshal(doc.Input, &sv); err == nil && sv != "" {
			b, err := json.Marshal(sv)
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}

// deriveRequestIDs builds the correlation ids for one upstream request.
func deriveRequestIDs(body []byte) requestIDs {
	seed := conversationSeed(body)
	if seed == "" {
		seed = "fallback_" + randomHex32()
	}
	return requestIDs{
		session: canonicalSessionID(seed),
		request: mintRequestID(),
		project: projectID(),
	}
}

func randomHex32() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// openCodeUserAgent is the desktop client's User-Agent (BP-219 capture:
// opencode/latest/2.2.2/desktop — App.useragent with the desktop's
// OPENCODE_CLIENT). The protocol argument is kept for call-site compatibility.
func openCodeUserAgent(protocol string) string {
	return fmt.Sprintf("opencode/latest/%s/desktop", openCodeClientVersion)
}

// disguiseHeaders returns the full header set every upstream request carries.
// protocol picks the ai-sdk segment of the User-Agent ("chat"/"responses"/
// "anthropic"); "" means chat. 2.2.x sends no x-opencode-request (the 1.x
// header): the capture shows exactly the ten fields model-request.ts sets.
func disguiseHeaders(ids requestIDs, protocol string) [][2]string {
	return [][2]string{
		{"user-agent", openCodeUserAgent(protocol)},
		{"x-opencode-client", "desktop"},
		{"x-opencode-session", ids.session},
		{"x-opencode-session-id", ids.session},
		{"x-session-affinity", ids.session},
		{"x-session-id", ids.session},
		{"x-opencode-project", ids.project},
		{"authorization", "Bearer " + AnonymousKey},
	}
}

// --- free-lane request shaping ---

// coreToolDefs are the desktop's own declarations for the five core tools,
// captured from a live 2.2.2 desktop request (BP-219, 2026-10-30) and embedded
// verbatim. The gate reads the tools' SHAPES: the earlier one-property stand-in
// schemas passed the 1.19-era gate but not this one (live matrix: real schemas
// + harness 200, stub schemas 429).
//
//go:embed assets/core_tools.json
var coreToolsJSON []byte

var coreToolOnce sync.Once
var coreToolChatDefs, coreToolResponsesDefs map[string]map[string]any

func coreToolDefs() (map[string]map[string]any, map[string]map[string]any) {
	coreToolOnce.Do(func() {
		var list []map[string]any
		if err := json.Unmarshal(coreToolsJSON, &list); err != nil {
			return // fall back to the stand-ins below
		}
		chatDefs := map[string]map[string]any{}
		responsesDefs := map[string]map[string]any{}
		for _, t := range list {
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				chatDefs[name] = map[string]any{"type": "function", "function": fn}
				responsesDefs[name] = map[string]any{
					"type": "function", "name": name,
					"description": fn["description"], "parameters": fn["parameters"],
				}
			}
		}
		coreToolChatDefs = chatDefs
		coreToolResponsesDefs = responsesDefs
	})
	return coreToolChatDefs, coreToolResponsesDefs
}

func gateToolChat(name string) map[string]any {
	chat, _ := coreToolDefs()
	if t := chat[name]; t != nil {
		return t
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": gateDescription,
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}},
		},
	}
}

func gateToolResponses(name string) map[string]any {
	_, responses := coreToolDefs()
	if t := responses[name]; t != nil {
		return t
	}
	return map[string]any{
		"type": "function", "name": name, "description": gateDescription,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}},
	}
}

// toolNameOf reads a tool's name from either wire shape.
func toolNameOf(tool map[string]any) string {
	if fn, ok := tool["function"].(map[string]any); ok {
		if s, ok := fn["name"].(string); ok {
			return s
		}
	}
	if s, ok := tool["name"].(string); ok {
		return s
	}
	return ""
}

// toolsOfAny normalizes body["tools"] to []any regardless of how the caller
// built it. A plain []any assertion used to fail for a concretely-typed
// []map[string]any slice and silently replaced every real tool with the gate
// stubs — the worst kind of bug, because the request still returned 200.
func toolsOfAny(v any) []any {
	switch ts := v.(type) {
	case []any:
		return ts
	case []map[string]any:
		out := make([]any, 0, len(ts))
		for _, t := range ts {
			out = append(out, t)
		}
		return out
	default:
		return nil
	}
}

// ensureGateTools injects the missing gate tools into an already-built wire
// body (map form). Chat lane with no tools at all also gets
// tool_choice:"none" so the model does not call the stubs; the Responses lane
// must never receive tool_choice:"none" (upstream 400) — an existing "none"
// is stripped instead. Returns the number of injected tools.
func ensureGateTools(body map[string]any, responsesLane bool) int {
	tools := toolsOfAny(body["tools"])
	wasEmpty := len(tools) == 0
	injected := 0
	for _, gate := range gateToolNames {
		found := false
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok && toolNameOf(tm) == gate {
				found = true
				break
			}
		}
		if !found {
			if responsesLane {
				tools = append(tools, gateToolResponses(gate))
			} else {
				tools = append(tools, gateToolChat(gate))
			}
			injected++
		}
	}
	if injected == 0 {
		return 0
	}
	body["tools"] = tools
	if wasEmpty && !responsesLane {
		body["tool_choice"] = "none"
	}
	if responsesLane {
		if s, ok := body["tool_choice"].(string); ok && s == "none" {
			delete(body, "tool_choice")
		}
	}
	return injected
}

// gateToolSet is the set of names we may have injected; calls to them are
// stripped from the response before it reaches the client.
var gateToolSet = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range gateToolNames {
		m[n] = true
	}
	return m
}()

// isGateToolName reports whether name is one of the free-lane gate tools.
func isGateToolName(name string) bool { return gateToolSet[name] }
