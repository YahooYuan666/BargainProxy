// Package trae implements the Trae CN (SOLO) channel: OpenAI-in,
// SOLO-llm_utils_chat-out with forced streaming, serial per-account
// concurrency (official free-tier limit), daily checkin and credit queries.
//
// Behaviour notes (clean-room from research/2026-10-13-三项目与生态调研.md):
// the upstream chat endpoint is /api/agent/v3/llm_utils_chat on the SOLO
// agent host with a fixed header set. BOTH `model` and `config_name` are sent
// and must carry the same id: config_name is what the upstream routes on, and
// a request without it answers 200 from a single fixed provider endpoint
// regardless of the model asked for (BP-137 — measured, see buildSOLOBody).
// Error 1005 means plan entitlement exhaustion (hard cooldown), 401 means the
// IDE JWT died.
package trae

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// IDE client identity constants (must match the official IDE handshake).
const (
	ideVersion     = "0.1.52"
	ideVersionCode = "20260811"
	appID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	deviceBrand    = "83DG"
	osVersion      = "Windows 11 Pro"
	functionSOLO   = "solo_work_lite"

	// traeToolInputCeiling is the measured input ceiling of the tool-bearing
	// surface (llm_utils_chat). Field evidence 2026-10-23: binary search shows
	// 261623 tokens pass and ~264000 answer 4026 "context length has exceeded
	// the maximum limit"; the same wall holds for every model (glm-5.3-flash
	// and deepseek-v4.1-flash both stop at the same number), and the vendor
	// catalogue reports exactly this value as context_window_tokens.dev.
	//
	// A tool-carrying prompt above it can never succeed on this channel, so the
	// driver refuses it before dispatch rather than burning a leg on a certain
	// 4026 (measured: the refusal takes ~4s and often ~2 minutes on big
	// prompts).
	traeToolInputCeiling = 262144

	// functionsTrae is the ladder of SOLO functions, tried in order. A model
	// or tool set that one function refuses is often taken by the next: the
	// upstream answers 400 with a business code (4001/4023/1005) rather than
	// "this model does not exist", so the caller must walk the ladder instead
	// of failing the turn (magpie-community/plugins packages/trae, 2026-10-21;
	// field 2026-10-21 msg_5cdb126709: same session answered 200 seconds
	// earlier on a different function).
)

// functionsTrae is the ordered ladder; the first entry is the preferred one.
//
// The last two entries were added 2026-10-25 after re-reading the catalogue we
// captured on 2026-10-23 (research/2026-10-24-trae-dual-channel-1m.md §2.1):
// every model publishing context_window_tokens {dev:200000, max:1000000} lists
// itself under `fn=solo_agent_remote`, while the ladder we shipped only ever
// asked four functions — the one the catalogue points at was never tried, so
// "we tried every function" did not actually include it.
//
// `solo_coder` comes from the same reading (magpie-community/plugins 0.2.20
// lists it beside the other four): it carries models the others do not
// (glm-5v-turbo, kimi-k2.5, minimax-m2.7, qwen-3.6-plus).
//
// Both sit LAST on purpose. The ladder is only walked when a function refuses
// with a business code (4001/4023/1005), so a new tail entry can only add a
// fallback; it never changes which function answers a request the current
// ladder already serves. Whether solo_agent_remote actually lifts the 262144
// input ceiling on llm_utils_chat is NOT verified — the catalogue only says
// which function lists those models. If it does not, the 4026 lands where it
// lands today and the turn fails exactly as before.
var functionsTrae = []string{
	functionSOLO, "chat_v3", "solo_agent", "solo_agent_lite",
	"solo_agent_remote", "solo_coder",
}

// Creds is the persisted credential shape for a trae account.
type Creds struct {
	AccessToken  string `json:"access_token"` // Cloud-IDE-JWT
	RefreshToken string `json:"refresh_token,omitempty"`
	UID          string `json:"uid,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

// Driver implements driver.Driver for the trae channel.
type Driver struct {
	cfg  config.TraeSettings
	http *http.Client // streaming chat calls (no total timeout + header/idle watchdogs)
	rpc  *http.Client // short checkin / usage calls (total timeout)
}

// New builds the driver.
func New(cfg config.TraeSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	return &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient()}
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelTrae }

// ParseCreds decodes a credential blob.
func ParseCreds(raw []byte) (*Creds, error) {
	var c Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("trae: parse creds: %w", err)
	}
	return &c, nil
}

func (d *Driver) agentBase() string { return strings.TrimRight(d.cfg.AgentHost, "/") }
func (d *Driver) ugBase() string    { return strings.TrimRight(d.cfg.UgHost, "/") }

// soloHeaders sets the mandatory SOLO header set.
func soloHeaders(req *http.Request, c *Creds, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "Trae/"+ideVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	req.Header.Set("X-Cloudide-Token", c.AccessToken)
	req.Header.Set("X-Ide-Token", c.AccessToken)
	if c.UID != "" {
		req.Header.Set("X-Uid", c.UID)
	}
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", ideVersion)
	req.Header.Set("X-Ide-Version-Code", ideVersionCode)
	req.Header.Set("X-App-Version-Code", ideVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", osVersion)
	req.Header.Set("X-Device-Brand", deviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if c.MachineID != "" {
		req.Header.Set("X-Machine-Id", c.MachineID)
	}
	if c.DeviceID != "" {
		req.Header.Set("X-Device-Id", c.DeviceID)
	}
}

// ugHeaders sets the checkin/usage header set. It must read as the IDE's
// growth page: an Electron-shell User-Agent (Bun/script-shaped UAs are
// answered 9074 no matter how seldom the check-in is asked —
// yetone/magpie#808) and the account's device id in the 16-digit shape
// Trae CN takes (magpie-community/plugins#194).
func ugHeaders(req *http.Request, c *Creds) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ugUserAgent)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	if id := ugDeviceID(c.DeviceID, c.UID); id != "" {
		req.Header.Set("X-Device-Id", id)
	}
}

// ugUserAgent is the TRAE SOLO CN Electron shell the growth pages go out
// with (magpie 0.2.19 ugUA; site.clientVersion 0.1.69).
const ugUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) TRAE-SOLO-CN/0.1.69 Chrome/120.0.6099.291 Electron/28.2.10 Safari/537.36"

// ugDeviceID maps the stored device id onto the shape Trae CN's growth
// pages accept: an id already in the clients' shape (15–16 digits) passes
// through; anything else (19 digits from older logins, or nothing at all)
// is mapped onto a stable 16-digit id derived from the device id and uid.
// It is the account's own device in the accepted shape — never rotated,
// never another account's — so a 9095 ("this device is in for today") on
// it stays the day's answer (magpie 0.2.19 ugDevice, plugins#194).
func ugDeviceID(deviceID, uid string) string {
	id := deviceID
	if len(id) == 15 || len(id) == 16 {
		digitsOnly := true
		for _, c := range id {
			if c < '0' || c > '9' {
				digitsOnly = false
				break
			}
		}
		if digitsOnly {
			return id
		}
	}
	if id == "" && uid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("trae-cn-device\x00" + id + "\x00" + uid))
	hexv := hex.EncodeToString(sum[:])[:30]
	n, _ := new(big.Int).SetString(hexv, 16)
	if n == nil {
		return ""
	}
	// 1e15 + (n mod 9e15): a 16-digit number, first digit 1-9.
	n.Mod(n, big.NewInt(9_000_000_000_000_000))
	n.Add(n, big.NewInt(1_000_000_000_000_000))
	return n.String()
}

// Stream implements driver.Driver: OpenAI chat body -> SOLO body, forced
// stream, upstream SSE parsed into IR events.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	creds, err := ParseCreds(call.Account.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	// The channel has two surfaces with different ceilings (see sessions.go):
	// llm_utils_chat serves our tools but stops at 262144; chat_sessions reaches
	// 1M but runs the vendor's own agent and ignores our tool declarations.
	//
	// llm_utils_chat stays the DEFAULT because it is the only surface that
	// speaks our protocol (tool calls round-trip, sampling params, the function
	// ladder). The session surface is entered only when the request cannot fit
	// the ladder — that is the sole thing it can do better, and using it for
	// ordinary traffic would silently change how every answer is produced.
	est := estimateInputTokens(call.Request)
	hasTools := len(call.Request.Tools) > 0
	switch {
	case est <= traeToolInputCeiling:
		// Fits the ladder: unchanged behaviour.
	case hasTools:
		// A tool-carrying prompt above the ceiling can never succeed here
		// (sessions would drop every tool). Fail before dispatch so the router
		// cascades instead of paying a guaranteed 4026 — measured at ~4s, and
		// minutes on large prompts.
		return nil, uperr.New(uperr.ClassModelUnavailable, 400,
			"trae: %d-token prompt with tools exceeds this channel's %d-token tool ceiling; another channel is required",
			est, traeToolInputCeiling)
	case est > chatSessionMaxInput:
		return nil, uperr.New(uperr.ClassModelUnavailable, 400,
			"trae: %d-token prompt exceeds the %d-token session ceiling; another channel is required",
			est, chatSessionMaxInput)
	default:
		// Tool-free and too large for the ladder: the session surface serves it.
		return d.streamViaSessions(ctx, call, emit)
	}
	// Walk the SOLO function ladder: an upstream 400 whose business code says
	// "this function does not take this payload" is retried on the next
	// function, not reported as a bad request (relay parity, 2026-10-21).
	var resp *ir.ChatResponse
	var lastBody []byte
	lastFn := functionsTrae[0]
	for _, fn := range functionsTrae {
		body, berr := buildSOLOBody(call, fn)
		if berr != nil {
			return nil, uperr.New(uperr.ClassBadRequest, 400, "trae: build body: %v", berr)
		}
		lastBody, lastFn = body, fn
		resp, err = d.chatOnce(ctx, creds, body, call, emit)
		if err == nil {
			return resp, nil
		}
		if !traeWrongFunction(err) || ctx.Err() != nil {
			break
		}
		log.Printf("trae: function %s refused the payload (%v); trying the next", fn, err)
	}
	if err == nil {
		// The ladder was empty: nothing was attempted.
		return nil, uperr.New(uperr.ClassInternal, 0, "trae: no function attempted")
	}
	// Self-heal (ticket 37): the upstream rejects a stale/rotated JWT with
	// code=1001 ("not able to authenticate you") while the ug endpoints keep
	// answering — an imported token can silently expire this way. Force one
	// ExchangeToken rotation and retry the chat once; auth rejections happen
	// before any content event, so the retry cannot duplicate output.
	if ue, ok := uperr.As(err); ok && ue.Class == uperr.ClassAuth && call.Account != nil {
		log.Printf("trae: chat auth rejected (acct=%s), forcing token refresh and retrying once", call.Account.ID)
		if rerr := d.refreshToken(ctx, call.Account, true); rerr == nil {
			log.Printf("trae: token refresh ok (acct=%s), retrying chat", call.Account.ID)
			if nc, perr := ParseCreds(call.Account.Credentials()); perr == nil {
				body, berr := buildSOLOBody(call, lastFn)
				if berr != nil {
					return nil, err
				}
				_ = lastBody
				return d.chatOnce(ctx, nc, body, call, emit)
			}
		} else {
			log.Printf("trae: token refresh failed (acct=%s): %v", call.Account.ID, rerr)
			// Prefer the refresh error when it is auth-class: it carries the
			// actionable reason ("refresh token invalid — re-login required")
			// that the client and the panel should see instead of the bare 1001.
			if re, ok := uperr.As(rerr); ok && re.Class == uperr.ClassAuth {
				return nil, rerr
			}
		}
	}
	return nil, err
}

// chatOnce performs one llm_utils_chat attempt (open SSE → IR events).
func (d *Driver) chatOnce(ctx context.Context, creds *Creds, body []byte, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	if creds.AccessToken == "" {
		return nil, uperr.New(uperr.ClassAuth, 401, "trae: missing access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.agentBase()+"/api/agent/v3/llm_utils_chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	soloHeaders(req, creds, true)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	// A 200 whose content-type is JSON (not event-stream) is an error in a
	// 200: classify the business error it carries instead of feeding JSON
	// into the SSE parser and ending at "no terminal signal" (magpie 0.2.19:
	// "an answer that isn't a stream is an error in a 200").
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "event-stream") && strings.Contains(ct, "json") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if code, msg, ok := detectError(string(raw)); ok {
			return nil, classifyBusiness(code, msg)
		}
		return nil, uperr.New(uperr.ClassTransport, 200, "trae: non-stream 200 answer: %s", truncate(raw, 200))
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return parseSSE(ctx, streamBody, call, emit)
}

// traeSessionID derives a stable per-(account, conversation) UUID for the
// chat body: Trae bills/counts usage per session, so a multi-step
// conversation without one reads as a fresh session per request on the
// upstream usage page — which is also what makes our bill unverifiable
// against theirs (magpie 0.2.19 conversationID; account uid folded in so a
// failover inside one conversation is not read as another account's).
// Empty SessionKey (no stable identity) still yields a random UUID.
func traeSessionID(call driver.ChatCall) string {
	uid := ""
	if call.Account != nil {
		uid = call.Account.UID
		if uid == "" {
			uid = call.Account.ID
		}
	}
	seed := uid + "\x00" + call.Request.SessionKey
	if call.Request.SessionKey == "" {
		seed = uid + "\x00" + traeRequestID() // unstable: at least unique per attempt
	}
	sum := sha256.Sum256([]byte(seed))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // RFC 4122 version 5-shaped
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// traeRequestID is a fresh random UUID per attempt.
func traeRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		sum := sha256.Sum256([]byte(time.Now().String()))
		copy(b[:], sum[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildSOLOBody converts the IR request into the SOLO chat body:
// {messages, function:"solo_work_lite", stream:true, model}.
// traeEffort maps the portable gear onto Trae's levels. BP-165: the client
// bundle names FIVE levels (reasoning_effort.{low,medium,high,extra_high,max},
// handed out per model by remote config — plugins trae 0.2.23, issue #434),
// so medium and max now pass through as themselves; the old fold to
// high/extra_high threw away DeepSeek V4.1 Flash's max band. The server
// validates the level against the model's own config.
func traeEffort(effort string) string {
	switch effort {
	case "minimal", "low":
		return "light"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "extra_high"
	case "max":
		return "max"
	}
	return ""
}

func buildSOLOBody(call driver.ChatCall, function string) ([]byte, error) {
	req := call.Request
	var messages []map[string]any
	for _, m := range req.Messages {
		wire := map[string]any{"role": string(m.Role)}
		switch {
		case len(m.Content) == 0 && m.Role == ir.RoleAssistant && len(m.ToolCalls) > 0:
			// tool-only assistant message: content stays absent
		case len(m.Content) == 1 && m.Content[0].Kind == "text":
			wire["content"] = []map[string]any{{"type": "text", "text": m.Content[0].Text}}
		default:
			var parts []map[string]any
			for _, p := range m.Content {
				switch {
				case p.Kind == "text":
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				case p.Kind == "image" && p.ImageB64 != "":
					// OpenAI image_url shape, data URI: the multimodal models
					// take this (magpie 0.2.19 imageOf). Silent drops read as
					// "the model saw a different image" — a 200 with confident
					// nonsense nobody could trace (BP-98 TRAE-7).
					mime := p.ImageMIME
					if mime == "" {
						mime = "image/png"
					}
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:" + mime + ";base64," + p.ImageB64},
					})
				}
			}
			wire["content"] = parts
		}
		if m.Role == ir.RoleTool && len(m.Content) == 0 {
			// A tool result with no content: TRAE's SOLO endpoint answers
			// 400 "the param is invalid" for an empty/absent content field
			// on tool messages (field 2026-10-21, msg_5cdb126709 — the same
			// session passed seconds earlier; the delta was a repaired
			// pairing that left one tool result content-less). Give the
			// upstream the empty-string content it expects.
			wire["content"] = []map[string]any{{"type": "text", "text": "(no output)"}}
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				if strings.TrimSpace(tc.Name) == "" {
					continue // upstream requires function_call.name
				}
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function_call": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			if len(tcs) > 0 {
				wire["tool_calls"] = tcs
			}
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		if m.ToolName != "" {
			wire["name"] = m.ToolName
		}
		messages = append(messages, wire)
	}
	body := map[string]any{
		"messages": messages,
		"function": function,
		"stream":   true, // forced
		"model":    call.UpstreamModel,
		// config_name is what the upstream ROUTES on (BP-137). Sending only
		// `model` does not fail — the request answers 200 with a normal-looking
		// reply from ONE fixed provider endpoint, whatever the model says. The
		// proof is in the response's own timing_cost frame:
		//
		//	model=kimi-k3                 provider_model_name=ep-20260802210216-sl4tw
		//	model=Doubao-Seed-2.1-Pro     provider_model_name=ep-20260802210216-sl4tw
		//	model=kimi-k2.6               provider_model_name=ep-20260802210216-sl4tw
		//	model=glm-5.3-flash           provider_model_name=ep-20260802210216-sl4tw
		//	model=___not_a_model___       provider_model_name=ep-20260802210216-sl4tw
		//
		// (measured 2026-10-27 on the CN account; ep-… is a Volcengine endpoint
		// id, i.e. ByteDance's own default). Adding config_name changed the
		// answers to ali-kimi-k3 / glm-5.3-flash — the models actually asked
		// for. This also explains the long-standing "any fake model name gets
		// 200" observation: it was never leniency, it was the router ignoring
		// us. magpie sends config_name+model together (trae index.mjs
		// chatBody); traework documents that the chat route matches
		// config_name exactly.
		//
		// The old "config_name injection breaks newer models" note in this
		// package's header came from an early, partial read and is now known
		// to be wrong — the opposite was true.
		"config_name": call.UpstreamModel,
		"request_id":  traeRequestID(),
		"session_id":  traeSessionID(call),
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	// BP-132: Trae's own reasoning_effort levels are light/high/extra_high
	// (magpie trae 0.2.20) — the gear the client picked never reached the
	// upstream before, so every model ran at its default.
	if gear := traeEffort(req.RequestedEffort); gear != "" {
		body["reasoning_effort"] = gear
	}
	var tools []map[string]any
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue // upstream requires a function name; one nameless tool rejects the whole request
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if t.Parameters != nil {
			// The upstream Go struct types parameters as a JSON *string*
			// (OpenAI types it as an object): an object body answers
			// code=4001 "cannot unmarshal object into Go struct field
			// FunctionDefinition.tools.function.parameters of type string".
			if s, err := json.Marshal(t.Parameters); err == nil {
				fn["parameters"] = string(s)
			}
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		body["tools"] = tools
		if s, ok := req.ToolChoice.(string); ok {
			switch strings.ToLower(s) {
			case "none":
				delete(body, "tools")
			default:
				body["tool_choice"] = s
			}
		}
	}
	return json.Marshal(body)
}

// parseSSE parses the SOLO upstream stream. The wire is a named-event SSE
// (event-name lines; the event completes on the blank line):
//
//	event:output      data:{"response":"<content delta>","reasoning_content":"<thinking delta>","tool_calls":null|{…}|[…]}
//	event:token_usage data:{"prompt_tokens":…,"completion_tokens":…,"reasoning_tokens":…}
//	event:done        data:{"finish_reason":"stop"}
//	event:error       data:{"code":…,"message":…}
//
// metadata / timing_cost / extra_info carry no client content. Bare `data:`
// lines in the legacy OpenAI chunk shape are still accepted, but every trae
// model answers with named events — a chunks-only parser silently produced
// empty completions (live 2026-10-16).
func parseSSE(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: time.Now().Unix(), Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
		toolAcc    = map[int]*ir.ToolCall{}
		toolOrder  []int
		started    = map[int]bool{}
	)
	// Empty-completion forensics: keep the first frames and log them only when
	// nothing came out of the stream — a silent empty answer is otherwise a
	// mystery (this is how the 2026-10-16 shape drift was found).
	var rawFrames []string
	noteFrame := func(s string) {
		if len(rawFrames) < 6 {
			rawFrames = append(rawFrames, truncate([]byte(s), 300))
		}
	}

	// legacyChunk handles the old per-line OpenAI-shaped chunk.
	legacyChunk := func(payload string) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				ReasoningTokens  int `json:"reasoning_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				thinkBuf.WriteString(ch.Delta.ReasoningContent)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ch.Delta.ReasoningContent}); err != nil {
					return err
				}
			}
			if ch.Delta.Content != "" {
				textBuf.WriteString(ch.Delta.Content)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ch.Delta.Content}); err != nil {
					return err
				}
			}
			if ch.FinishReason != "" {
				terminated = true
				stop = mapFinish(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			usage = ir.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, ReasoningTokens: chunk.Usage.ReasoningTokens}
			gotUsage = true
		}
		return nil
	}

	// mergeToolDeltas folds SOLO tool-call fragments (null / object / array)
	// into the accumulators and streams start/args events. SOLO names the
	// function object `function_call`; OpenAI says `function` — both accepted.
	mergeToolDeltas := func(raw json.RawMessage) error {
		if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
			return nil
		}
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err != nil {
			var one map[string]any
			if json.Unmarshal(raw, &one) != nil {
				return nil
			}
			arr = []map[string]any{one}
		}
		for _, tc := range arr {
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			acc, ok := toolAcc[idx]
			if !ok {
				acc = &ir.ToolCall{}
				toolAcc[idx] = acc
				toolOrder = append(toolOrder, idx)
			}
			if v, ok := tc["id"].(string); ok && v != "" {
				acc.ID = v
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				fn, _ = tc["function_call"].(map[string]any)
			}
			var frag string
			if fn != nil {
				if v, ok := fn["name"].(string); ok && v != "" {
					acc.Name = v
				}
				if v, ok := fn["arguments"].(string); ok && v != "" {
					frag = v
					acc.Arguments = mergeArgs(acc.Arguments, v)
				}
			}
			switch {
			case !started[idx] && (acc.Name != "" || acc.ID != ""):
				started[idx] = true
				if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: idx, ToolCall: &ir.ToolCall{ID: acc.ID, Name: acc.Name}}); err != nil {
					return err
				}
				if acc.Arguments != "" {
					// Flush fragments that arrived before the id/name did.
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: idx, Delta: acc.Arguments}); err != nil {
						return err
					}
				}
			case started[idx] && frag != "":
				if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: idx, Delta: frag}); err != nil {
					return err
				}
			}
		}
		return nil
	}

	handleEvent := func(name, payload string) error {
		switch name {
		case "output":
			var ev struct {
				Response         string          `json:"response"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			}
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				return nil
			}
			if ev.ReasoningContent != "" {
				thinkBuf.WriteString(ev.ReasoningContent)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ev.ReasoningContent}); err != nil {
					return err
				}
			}
			if ev.Response != "" {
				textBuf.WriteString(ev.Response)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ev.Response}); err != nil {
					return err
				}
			}
			return mergeToolDeltas(ev.ToolCalls)
		case "token_usage":
			// Cache accounting (2026-10-25): SOLO reports the prefix-cache hit
			// at the TOP level of token_usage as `cache_read_input_tokens`,
			// not in the OpenAI `prompt_tokens_details.cached_tokens` slot that
			// every other driver reads. This driver only ever decoded the three
			// counts, so Trae's cache column read "unknown" for every request
			// even though the upstream was reporting a hit — the channel looked
			// like it did not publish cache data at all (misakano7545 11b57d1,
			// 2026-10-21, reports the same field for the same surface).
			//
			// Both spellings are accepted so a future upstream move to the
			// OpenAI shape keeps working. CacheKnown is set only when a field is
			// PRESENT: an absent field means "not reported", which the panel
			// must show as unknown rather than as a 0% hit rate (BP-53).
			var u struct {
				PromptTokens         int  `json:"prompt_tokens"`
				CompletionTokens     int  `json:"completion_tokens"`
				ReasoningTokens      int  `json:"reasoning_tokens"`
				CacheReadInputTokens *int `json:"cache_read_input_tokens"`
				PromptTokensDetails  *struct {
					CachedTokens *int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			}
			if err := json.Unmarshal([]byte(payload), &u); err != nil {
				return nil
			}
			usage = ir.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, ReasoningTokens: u.ReasoningTokens}
			switch {
			case u.CacheReadInputTokens != nil:
				usage.CachedInputTokens = *u.CacheReadInputTokens
				usage.CacheKnown = true
			case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil:
				usage.CachedInputTokens = *u.PromptTokensDetails.CachedTokens
				usage.CacheKnown = true
			}
			gotUsage = true
			return nil
		case "done":
			terminated = true
			var d struct {
				FinishReason string `json:"finish_reason"`
			}
			if err := json.Unmarshal([]byte(payload), &d); err != nil {
				return nil
			}
			if d.FinishReason != "" {
				stop = mapFinish(d.FinishReason)
			}
			return nil
		case "error":
			if code, msg, ok := detectError(payload); ok {
				return classifyBusiness(code, msg)
			}
			return uperr.New(uperr.ClassInternal, 200, "trae: stream error: %s", truncate([]byte(payload), 200))
		default:
			if name != "" {
				return nil // metadata / timing_cost / extra_info: no client content
			}
			// Bare payload without an event name: legacy chunk, or a wrapped
			// business error body.
			if code, msg, ok := detectError(payload); ok {
				return classifyBusiness(code, msg)
			}
			return legacyChunk(payload)
		}
	}

	var (
		evName string
		evData strings.Builder
	)
	flush := func() error {
		name, payload := evName, strings.TrimSpace(evData.String())
		evName = ""
		evData.Reset()
		if name == "" && payload == "" {
			return nil
		}
		noteFrame(name + "|" + payload)
		return handleEvent(name, payload)
	}

scan:
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "trae: stream canceled: %v", err)
		}
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "[DONE]":
			terminated = true
			break scan
		case line == "":
			if err := flush(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "event:"):
			evName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if evName == "" {
				// Bare data line: a complete legacy event on its own.
				if payload == "[DONE]" {
					terminated = true
					break scan
				}
				noteFrame("|" + payload)
				if err := handleEvent("", payload); err != nil {
					return nil, err
				}
				continue
			}
			evData.WriteString(payload)
		case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, ":"):
			// Frame id / SSE comment: ignored.
		default:
			// Bare JSON without the data: prefix (legacy shape): its own event.
			if err := flush(); err != nil {
				return nil, err
			}
			noteFrame("|" + trimmed)
			if err := handleEvent("", trimmed); err != nil {
				return nil, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"trae: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "trae: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "trae: stream read: %v", err)
	}
	// A final event without its trailing blank line still counts.
	if err := flush(); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "trae: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "trae: stream ended without terminal signal")
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *toolAcc[idx])
	}
	if len(out.Message.ToolCalls) > 0 && stop == ir.StopEnd {
		stop = ir.StopToolUse
	}
	if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolOrder) == 0 {
		log.Printf("trae: empty completion model=%s account=%s frames=%q",
			call.UpstreamModel, call.Account.ID, rawFrames)
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.StopReason = stop
	out.Usage = usage
	if gotUsage {
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &usage}); err != nil {
			return nil, err
		}
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: stop}); err != nil {
		return nil, err
	}
	return out, nil
}

// mapFinish maps a SOLO finish_reason onto the IR stop reason.
func mapFinish(reason string) ir.StopReason {
	switch strings.ToLower(reason) {
	case "stop":
		return ir.StopEnd
	case "length":
		return ir.StopLength
	case "tool_calls":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default:
		return ir.StopEnd
	}
}

// mergeArgs folds one tool-arguments frame into what has accumulated so far.
// SOLO sends the arguments as snapshots that grow, not always as fragments:
// a frame may restate "everything so far" (also re-punctuated), so a blind
// concatenation welds snapshots into broken JSON and the agent tool turn
// dies downstream after a 200. Translation of magpie-community/plugins
// packages/trae 0.2.19 (merge inside parts() + resent() + done()):
//   - empty / stale (prev already contains next): keep prev;
//   - next extends prev, or prev is already a whole JSON value: take next;
//   - next is a whole object re-sending the call under prev's first key
//     while prev+next is not whole (plugins#24, re-punctuated resend): take next;
//   - otherwise a true fragment: concatenate.
func mergeArgs(prev, next string) string {
	if next == "" || strings.HasPrefix(prev, next) {
		return prev
	}
	if strings.HasPrefix(next, prev) || wholeJSON(prev) {
		return next
	}
	if resentSnapshot(prev, next) {
		return next
	}
	return prev + next
}

// wholeJSON reports whether s is already a complete JSON value.
func wholeJSON(s string) bool {
	if s == "" {
		return false
	}
	return json.Valid([]byte(s))
}

// resentSnapshot reports whether next is the call's whole arguments sent
// again after a partial snapshot — not a piece of them: SOLO resends a
// snapshot whose punctuation is not the first one's, so it does not start
// with prev. A whole object under the same first key as prev's, which prev
// and it joined do not make whole (magpie 0.2.19 resent()).
func resentSnapshot(prev, next string) bool {
	firstKey := func(s string) string {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if !strings.HasPrefix(s, "{") {
			return ""
		}
		var key string
		dec := json.NewDecoder(strings.NewReader(s))
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return ""
		}
		tok, err = dec.Token()
		if err != nil {
			return ""
		}
		if k, ok := tok.(string); ok {
			key = k
		}
		return key
	}
	nt := strings.TrimLeftFunc(next, unicode.IsSpace)
	if !strings.HasPrefix(nt, "{") || !wholeJSON(nt) || wholeJSON(prev+next) {
		return false
	}
	k := firstKey(prev)
	return k != "" && k == firstKey(next)
}

func detectError(payload string) (int, string, bool) {
	var doc struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		// The nested shapes below ({error:{code,message}} and the ByteDance
		// envelope {ResponseMetadata:{Error:{Code,Message}}}) used to fall
		// into the legacy-chunk parser and end as "stream ended without
		// terminal signal" — a business refusal scored as a network problem
		// (BP-98 TRAE-9, magpie 0.2.19 errorOf).
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return 0, "", false
	}
	if code, msg, ok := traeCodeOf(doc.Code, doc.Message); ok {
		return code, msg, true
	}
	var nested struct {
		Error struct {
			Code    any    `json:"code"`
			CodeAlt string `json:"Code"`
			Message string `json:"message"`
			MsgAlt  string `json:"Message"`
		} `json:"error"`
		Meta struct {
			Error struct {
				Code    any    `json:"Code"`
				CodeAlt any    `json:"code"`
				Message string `json:"Message"`
				MsgAlt  string `json:"message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if err := json.Unmarshal([]byte(payload), &nested); err != nil {
		return 0, "", false
	}
	if nested.Error.Code != nil || nested.Error.CodeAlt != "" {
		msg := nested.Error.Message
		if msg == "" {
			msg = nested.Error.MsgAlt
		}
		if code, _, ok := traeCodeOf(nested.Error.Code, msg); ok {
			return code, msg, true
		}
		if code, _, ok := traeCodeOf(nested.Error.CodeAlt, msg); ok {
			return code, msg, true
		}
	}
	if nested.Meta.Error.Code != nil || nested.Meta.Error.CodeAlt != nil {
		msg := nested.Meta.Error.Message
		if msg == "" {
			msg = nested.Meta.Error.MsgAlt
		}
		if code, _, ok := traeCodeOf(nested.Meta.Error.Code, msg); ok {
			return code, msg, true
		}
		if code, _, ok := traeCodeOf(nested.Meta.Error.CodeAlt, msg); ok {
			return code, msg, true
		}
	}
	return 0, "", false
}

// traeCodeOf normalizes a business code (number or digit string) and its
// message; code 0 / absent is not an error.
func traeCodeOf(raw any, msg string) (int, string, bool) {
	switch t := raw.(type) {
	case float64:
		if int(t) != 0 {
			return int(t), msg, true
		}
	case string:
		if t == "" {
			return 0, "", false
		}
		code := 0
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0, "", false
			}
			code = code*10 + int(c-'0')
		}
		if code != 0 {
			return code, msg, true
		}
	}
	return 0, "", false
}

// traeWrongFunction reports whether the failure means "this SOLO function
// will not take this payload", which is retried on the next function rather
// than shown to the caller. Judged on the upstream business code carried in
// ue.Code (magpie-community/plugins packages/trae 0.2.19:
// wrongFunction = ["4001","4023","1005"] by code) — the 1005 branch of
// classifyBusiness words its message "plan entitlement exhausted" without the
// digits, so a substring scan misses it and 4023 never had a branch at all
// (BP-98 TRAE-1, red first in ladder_codes_test.go).
func traeWrongFunction(err error) bool {
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassBadRequest && ue.Class != uperr.ClassSoftRate && ue.Class != uperr.ClassModelRate {
		return false
	}
	if ue.Code != "" {
		return ue.Code == "4001" || ue.Code == "4023" || ue.Code == "1005"
	}
	// A plain "the param is invalid" 400 from this endpoint is the function
	// refusing the shape more often than not; walking the ladder costs one
	// cheap request and a wrong guess is still bounded by it.
	return strings.Contains(strings.ToLower(ue.Message), "param is invalid")
}

func classifyBusiness(code int, msg string) *uperr.Error {
	switch code {
	case 1001:
		// "We're sorry, but we are not able to authenticate you": the JWT was
		// rejected (stale/rotated). ClassAuth so Stream can self-heal with a
		// forced token exchange (ticket 37).
		return uperr.New(uperr.ClassAuth, 401, "trae: %s", msg)
	case 1005, 4008, 4011:
		// The plan-entitlement family: 1005 CN/Global, 4008 CN, 4011 Global
		// (magpie 0.2.19 site tables). Field evidence from traework2api-panel
		// (bb1059a/41217d3, 2026-10-22): a 1005 means THIS MODEL is behind a
		// paywall for the account's plan — the same account serves dozens of
		// other models fine. The old account-wide 12h rest froze a healthy
		// account (misakano saw one stay dead for 17 days of retries), so the
		// code is model-scoped now (ClassModelRate: the pool fills in the
		// request's model when e.Model is empty) with a 1h TTL, matching
		// upstream planDeniedTTL. The code still rides on ue.Code for the
		// ladder gate and the panel.
		e := uperr.New(uperr.ClassModelRate, 200, "trae: plan entitlement exhausted (code=%d): %s", code, msg)
		e.Code = fmt.Sprint(code)
		e.ResetAt = time.Now().Add(time.Hour)
		return e
	case 4001:
		// Upstream rejected the payload shape ("bad request: json: cannot
		// unmarshal …"): the account is fine, the body was not — no penalty,
		// no rotation (mirrors workbuddy's 11101). The code stays in the
		// message so the caller can tell a function refusal (retried on the
		// next ladder rung) from a genuinely broken payload.
		e := uperr.New(uperr.ClassBadRequest, 400, "trae: code=4001 %s", msg)
		e.Code = "4001"
		return e
	case 4023:
		// "Model not available on this function": the other half of the
		// function-refusal pair (magpie 0.2.19 wrongFunction). ClassBadRequest
		// (account is fine) + code on ue.Code so the ladder walks on.
		e := uperr.New(uperr.ClassBadRequest, 400, "trae: code=4023 %s", msg)
		e.Code = "4023"
		return e
	default:
		return uperr.New(uperr.ClassInternal, 200, "trae: code=%d %s", code, msg)
	}
}

func classifyHTTP(status int, body []byte) *uperr.Error {
	if code, msg, ok := detectError(string(body)); ok {
		return classifyBusiness(code, msg)
	}
	switch {
	case status == 401:
		return uperr.New(uperr.ClassAuth, status, "trae: %s", truncate(body, 300))
	case status == 429:
		return uperr.New(uperr.ClassSoftRate, status, "trae: %s", truncate(body, 300))
	case status == 404:
		// The path/model is missing upstream: identical on every account of
		// this channel, so this is a binding property, not a rate limit. It
		// used to be classified as model_rate (60s ResetAt), which cooled the
		// account's model instead of just pausing the route (2026-10-19).
		return uperr.New(uperr.ClassNotFound, status, "trae: %s", truncate(body, 300))
	case status == 413:
		return uperr.New(uperr.ClassBadRequest, status, "trae: %s", truncate(body, 300))
	case status == 403:
		if uperr.IsEdgeBlock(body) {
			return uperr.EdgeBlocked(status, "trae: edge blocked: %s", truncate(body, 300))
		}
		return uperr.New(uperr.ClassWAF, status, "trae: %s", truncate(body, 300))
	case status == 400:
		return uperr.New(uperr.ClassBadRequest, status, "trae: %s", truncate(body, 300))
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "trae: %s", truncate(body, 300))
	default:
		return uperr.New(uperr.ClassInternal, status, "trae: %s", truncate(body, 300))
	}
}

// traeCheckinStatus is the check-in status page's answer.
type traeCheckinStatus struct {
	CheckedIn bool
	Credits   int64
	Enable    bool
}

// checkinStatus asks the check-in status page. It is called twice per run
// (before the claim and, since BP-147, after it) — the second read is what
// tells a real claim from an answer that registered nothing.
//
// The growth pages go out with {"req_source":1}, not {} — every check-in sent
// with {} came back 9074 even with the IDE's headers (yetone/magpie#808).
func (d *Driver) checkinStatus(ctx context.Context, creds *Creds, accID string) (traeCheckinStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ugBase()+"/trae/api/v2/ug/checkin_credits/status", bytes.NewReader(ugBody))
	if err != nil {
		return traeCheckinStatus{}, err
	}
	ugHeaders(req, creds)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return traeCheckinStatus{}, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae checkin status: %v", err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return traeCheckinStatus{}, classifyHTTP(resp.StatusCode, raw)
	}
	// The status/credits below feed task decisions; a truncated read must not
	// read as "not checked in, zero credits" (ticket 11).
	if rerr != nil {
		log.Printf("trae: checkin status read failed (account=%s, endpoint=checkin_credits/status): %v", accID, rerr)
		return traeCheckinStatus{}, uperr.New(uperr.ClassTransport, 0, "trae checkin status: read body: %v", rerr)
	}
	var st struct {
		Code      any    `json:"code"`
		Message   string `json:"message"`
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
	}
	_ = json.Unmarshal(raw, &st)
	// A 200 with a business code in the status body is not a status answer:
	// 1001 ("not able to authenticate") used to fall through to
	// "checked_in=false, enable=false" and the task ledger recorded "ok",
	// masking a dead credential forever (BP-98 TRAE-3; the chat path's
	// self-heal from ticket 37 never covered check-in).
	if code, _, ok := traeBodyCode(st.Code, st.Message); ok {
		return traeCheckinStatus{}, classifyBusiness(code, st.Message)
	}
	return traeCheckinStatus{CheckedIn: st.CheckedIn, Credits: st.Credits, Enable: st.Enable}, nil
}

// Checkin performs the daily checkin (idempotent upstream-side).
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) (checkedIn bool, credits int64, enable bool, err error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return false, 0, false, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	st, err := d.checkinStatus(ctx, creds, acc.ID)
	if err != nil {
		return false, 0, false, err
	}
	if st.CheckedIn || !st.Enable {
		// enable=false means the check-in program is unavailable for this
		// account — reporting it as "already checked in" masked auth failures
		// (ticket 37); callers now see the distinction.
		return st.CheckedIn, st.Credits, st.Enable, nil
	}
	// Claim. Retry only what retrying can fix (5xx / transport): a business
	// refusal is answered again by the same refusal.
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		lastErr = d.claimCheckin(ctx, creds)
		if lastErr == nil {
			// BP-147: a claim's own code 0 is not proof the day is in.
			// Trae answers it for a claim that registered nothing, and its
			// body usually carries no credits at all (magpie trae 0.2.22,
			// #333). Success is what the STATUS page says after the claim, so
			// the credits we report are the new ones, not the pre-claim read.
			back, serr := d.checkinStatus(ctx, creds, acc.ID)
			if serr != nil {
				// Unreadable status is not a failed claim: the claim itself
				// succeeded, we just cannot confirm or price it. Report the
				// success and keep the old balance rather than turning a
				// possible credit into a red task row.
				log.Printf("trae: checkin claimed but the status re-read failed (account=%s): %v", acc.ID, serr)
				return true, st.Credits, true, nil
			}
			if !back.CheckedIn {
				// Claimed code 0, day still not registered: report it honestly
				// as ineligible so the ledger does not claim money that never
				// arrived, and do not re-send the claim today (the upstream
				// answers the same way).
				return false, 0, true, &checkinCodeErr{
					ue: uperr.New(uperr.ClassBadRequest, 200,
						"trae: claim answered success but the day's check-in is not in — not retrying today"),
					code: 9101,
				}
			}
			return true, back.Credits, true, nil
		}
		if idempotent, ok := idempotentCheckinErr(lastErr); ok && idempotent {
			// 9095 "this device has checked in today": the day's success —
			// no retries, no red task row (magpie 0.2.19: outcome done).
			return true, st.Credits, true, nil
		}
		if !retryableCheckinErr(lastErr) && !rushCheckinErr(lastErr) {
			return false, 0, false, lastErr
		}
		// BP-135: 9074 "当前参与用户太多，请稍后再试" is the upstream asking
		// for a LATER retry — it is the check-in rush hour, not the "check in
		// via the app" refusal. It failed the task three days running at the
		// 01:00 slot while the message itself says the money is still there.
		// Wait out the rush with a wider spacing than the transport backoff.
		if attempt < 3 {
			delay := checkinRetryDelay
			if rushCheckinErr(lastErr) {
				delay = checkinRushRetryDelay
			}
			select {
			case <-ctx.Done():
				return false, 0, false, uperr.New(uperr.ClassCanceled, 0, "trae checkin: %v", ctx.Err())
			case <-time.After(delay):
			}
		}
	}
	return false, 0, false, lastErr
}

// checkinRushRetryDelay is the spacing for the 9074 rush answer. 20s kept
// landing inside the same rush (three days of failed 01:00 rows); a minute
// gives the upstream's own "稍后再试" a chance to be true. Package var so
// tests can shorten it.
var checkinRushRetryDelay = 60 * time.Second

// rushCheckinErr reports whether a claim refusal is the TRANSIENT rush shape:
// 9074 whose message asks for a later retry. Other 9074 wordings (the
// historical "go check in via the app" answer) stay fail-fast.
func rushCheckinErr(err error) bool {
	ce, ok := err.(*checkinCodeErr)
	if !ok || ce.code != 9074 {
		return false
	}
	return strings.Contains(ce.ue.Message, "参与用户太多")
}

// checkinRetryDelay is a package var so tests can shorten the backoff.
var checkinRetryDelay = 20 * time.Second

// ugBody is the body every ug growth page is sent with (BP-98 TRAE-2).
var ugBody = []byte(`{"req_source":1}`)

// checkinCodeErr carries the upstream business code of a failed claim so the
// caller can tell the day's idempotent answer from a real refusal. The
// uperr.Error sits in a named field: embedding it would collide with its
// Error string field.
type checkinCodeErr struct {
	ue   *uperr.Error
	code int
}

func (e *checkinCodeErr) Error() string { return e.ue.Error() }
func (e *checkinCodeErr) Unwrap() error { return e.ue }

// claimCheckin performs one claim attempt and verifies the business code.
func (d *Driver) claimCheckin(ctx context.Context, creds *Creds) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ugBase()+"/trae/api/v2/ug/checkin_credits/claim", bytes.NewReader(ugBody))
	if err != nil {
		return err
	}
	ugHeaders(req, creds)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae checkin claim: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyHTTP(resp.StatusCode, raw)
	}
	// A 200 can still carry a business error; an empty or non-JSON body keeps
	// the historical success semantics (some deployments answer `{}`).
	var env struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &env) == nil {
		if code, _, ok := traeBodyCode(env.Code, env.Message); ok {
			// A claim refusal is upstream's answer about today, not a network
			// problem and not an internal error: ClassBadRequest keeps it out
			// of the retryable set (a 9074 "check in via the app" answers the
			// same however often it is asked) and out of the pool's failure
			// scoring (check-in does not ride the chat pool).
			e := uperr.New(uperr.ClassBadRequest, 200, "trae checkin claim code=%v: %s", env.Code, env.Message)
			return &checkinCodeErr{ue: e, code: code}
		}
	}
	return nil
}

// traeBodyCode extracts a non-zero numeric business code from a 200 body
// (code arrives as number or digit-string; 0/absent = success).
func traeBodyCode(raw any, _ string) (int, string, bool) {
	switch t := raw.(type) {
	case float64:
		if int(t) != 0 {
			return int(t), "", true
		}
	case string:
		code := 0
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0, "", false
			}
			code = code*10 + int(c-'0')
		}
		if code != 0 {
			return code, "", true
		}
	}
	return 0, "", false
}

// idempotentCheckinErr reports whether the claim failure is actually the
// day's success: 9095 "this device has checked in today" (magpie 0.2.19
// reads it as outcome "ineligible" on the device / done for the account).
func idempotentCheckinErr(err error) (bool, bool) {
	if ce, ok := err.(*checkinCodeErr); ok {
		if ce.code == 9095 {
			return true, true
		}
		return false, true
	}
	return false, false
}

// retryableCheckinErr reports whether the claim failure is the kind a retry
// can fix: transport problems and 5xx. Business refusals (9074 "check in
// via the app" among them) answer the same however often they are asked;
// retrying them only burned 3×20s per account per slot and made the traffic
// look scripted (BP-98 TRAE-3).
func retryableCheckinErr(err error) bool {
	ue, ok := uperr.As(err)
	if !ok {
		return true // unknown error shape: keep the historical retry behavior
	}
	switch ue.Class {
	case uperr.ClassTransport:
		return true
	case uperr.ClassInternal:
		return true
	default:
		return false
	}
}

// Probe implements driver.Prober: fetch credit balance.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ugBase()+"/trae/api/v2/pay/ide_user_ent_usage", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	ugHeaders(req, creds)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae usage: %v", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	// Credit balance feeds pool scoring; a truncated read must surface as an
	// error rather than a zero balance (ticket 11).
	if rerr != nil {
		log.Printf("trae: usage probe read failed (account=%s, endpoint=ide_user_ent_usage): %v", acc.ID, rerr)
		return nil, uperr.New(uperr.ClassTransport, 0, "trae usage: read body: %v", rerr)
	}
	remain, limit, _, packs, earliestExpiry, unlimited, err := ParseEntUsageFull(raw)
	if err != nil {
		return nil, uperr.New(uperr.ClassInternal, 0, "trae usage parse: %v", err)
	}
	_ = packs
	// BP-132: the entitlement answer also names the account's real concurrency
	// allowance and pay identity (Trae2api-cn bb7b659). Surfaced in the note
	// for now — wiring it into per-account concurrency limits is a scheduler
	// decision that needs its own ticket.
	if pl, pay := ParseTraeEntitlementMeta(raw); pl > 0 || pay != "" {
		log.Printf("trae: entitlement meta (account=%s): parallel_limit=%d pay_identity=%s", acc.ID, pl, pay)
	}
	credits := &model.Credits{Remain: remain, Total: limit, UpdatedAt: time.Now()}
	if unlimited {
		// No finite quota: keep the routing floor and the 100% bar honest
		// instead of reading a full wallet as an empty one (BP-98 TRAE-5).
		credits.BalanceOnly = true
		credits.Total = remain
	}
	if !earliestExpiry.IsZero() {
		// Feeds the pool's burn-expiring-first weight (×3); trae never
		// supplied it before, so that weight silently never fired.
		credits.Expiring = remain
		credits.EarliestExpiresAt = earliestExpiry
	}
	return &driver.ProbeResult{Credits: credits, Note: "trae credits refreshed"}, nil
}

// ParseTraeEntitlementMeta reads the account-level fields the entitlement
// answer carries beside the packs (Trae2api-cn bb7b659): the per-account
// parallel limit and the pay identity (free/付费). Both are informational
// today; a future scheduler ticket can drive per-account concurrency from
// parallelLimit.
func ParseTraeEntitlementMeta(raw []byte) (parallelLimit int, payIdentity string) {
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					SoloAgentParallelLimit int    `json:"solo_agent_parallel_limit"`
					UserPayIdentity        string `json:"user_pay_identity"`
					UserPayIdentityStr     string `json:"user_pay_identity_str"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
		} `json:"user_entitlement_pack_list"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return 0, ""
	}
	for _, p := range resp.UserEntitlementPackList {
		q := p.EntitlementBaseInfo.Quota
		if q.SoloAgentParallelLimit > parallelLimit {
			parallelLimit = q.SoloAgentParallelLimit
		}
		if payIdentity == "" {
			payIdentity = q.UserPayIdentity
			if payIdentity == "" {
				payIdentity = q.UserPayIdentityStr
			}
		}
	}
	return parallelLimit, payIdentity
}

// ParseEntUsage aggregates the entitlement pack list: remain = limit - used.
func ParseEntUsage(raw []byte) (remain, limit, used int64, packs int, err error) {
	remain, limit, used, packs, _, _, err = ParseEntUsageFull(raw)
	return remain, limit, used, packs, err
}

// ParseEntUsageFull is ParseEntUsage with the two fields the pool needs and
// the old shape dropped (BP-98 TRAE-5, translated from magpie 0.2.19
// credits()):
//   - credits_limit === -1 is the UNLIMITED pack, not an empty one. The old
//     `if l <= 0 { continue }` registered a full account as Remain=0 and the
//     pool then kept it out of routing (the empty-account mirage); unlimited
//     callers read as available.
//   - each pack's end_time / expire_time returns as the earliest future
//     expiry so the pool can burn soon-to-die credits first.
func ParseEntUsageFull(raw []byte) (remain, limit, used int64, packs int, earliestExpiry time.Time, unlimited bool, err error) {
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
				EndTime    int64 `json:"end_time"`
				ExpireTime int64 `json:"expire_time"`
			} `json:"entitlement_base_info"`
			EndTime int64           `json:"end_time"`
			Usage   json.RawMessage `json:"usage"`
			Extra   map[string]any  `json:"-"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, 0, 0, 0, time.Time{}, false, err
	}
	const unlimitedPack = -1
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		u := usageCredits(p.Usage)
		switch {
		case l == unlimitedPack:
			// Unlimited: the usage counts as spent, the account stays available.
			unlimited = true
			used += u
			remain += 1<<62 - u // far from empty, without overflowing
			packs++
		case l > 0:
			limit += l
			used += u
			remain += l - u
			packs++
		default:
			continue // no quota on this pack
		}
		if end := normalizePackExpiry(p.EntitlementBaseInfo.EndTime, p.EntitlementBaseInfo.ExpireTime, p.EndTime); !end.IsZero() {
			if earliestExpiry.IsZero() || end.Before(earliestExpiry) {
				earliestExpiry = end
			}
		}
	}
	if remain < 0 {
		remain = 0
	}
	return remain, limit, used, packs, earliestExpiry, unlimited, nil
}

// usageCredits reads the pack's usage.credits_amount (a float on the wire).
func usageCredits(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var u struct {
		CreditsAmount float64 `json:"credits_amount"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0
	}
	if u.CreditsAmount <= 0 {
		return 0
	}
	return int64(u.CreditsAmount)
}

// normalizePackExpiry picks the first non-zero of (end_time, expire_time,
// pack-level end_time) and normalizes ms → s (values above 1e12 are ms).
func normalizePackExpiry(values ...int64) time.Time {
	for _, v := range values {
		if v <= 0 {
			continue
		}
		if v > 1e12 {
			v /= 1000
		}
		return time.Unix(v, 0)
	}
	return time.Time{}
}

// Models returns the curated model list (dynamic discovery uses
// get_detail_param during probes; the static list covers the core set).
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return []string{
		"Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Turbo", "Doubao-Seed-2.0-Code",
		"seed-code-pro-0430",
		"DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "DeepSeek-V4-Flash-Official",
		// Lower-case on purpose: TraeCode exposes this config_name verbatim and
		// the upstream matches it exactly. Sending the title-cased V4 spelling
		// makes Trae fall back to its default model, so the reply arrives from
		// something that is not DeepSeek at all (2026-10-21, Trae2api-cn's
		// model table documents the same constraint).
		"deepseek-v4.1-flash",
		"glm-5.2", "glm-5", "glm-5-turbo",
		"kimi-k3", "kimi-k3.1-pro", "kimi-k2.7-code", "kimi-k2.6",
		"minimax-m3", "qwen-3.7-plus",
	}, nil
}

// Refresh: Trae tokens come from the IDE OAuth exchange; the refresh token
// flow re-exchanges when expired.
// Trae OAuth exchange constants — mirror panel/trae_login.go (itself ported
// from the MIT reference's refreshLocked/ExchangeToken).
const (
	traeEpExchange  = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeOAuthClient = "en1oxy7wnw8j9n" // SOLO stable client id
)

// traeOAuthHost is a var (not a const) so tests can point it at a mock.
var traeOAuthHost = "https://api.trae.com.cn"

// traeRefreshSkew: only refresh when the JWT expires within this window (or
// its expiry is unknown). The reference refreshes daily at hour 3 with a 24h
// skew; our refresh task runs hourly, so a 24h skew against the ~24h TTL
// made EVERY tick re-exchange the token — 24 upstream rotations per account
// per day, each spending a refresh-token generation (BP-98 TRAE-6). Two
// hours keeps ~22h between rotations while still refreshing well before
// expiry.
const traeRefreshSkew = 2 * time.Hour

// refreshInFlight deduplicates concurrent refreshes of the same account:
// the refresh token is one-time, and two concurrent ExchangeToken calls on
// the same lineage make the loser answer 20101 ("refresh token is invalid")
// — which reads as a dead credential and can disable the account (BP-98
// TRAE-6). The first caller exchanges; the rest wait and keep their creds.
var refreshInFlight sync.Map // accountID -> *sync.WaitGroup-style gate

// Refresh rotates the Cloud-IDE JWT through ExchangeToken (refresh token
// rotates too — reference refreshLocked parity). This used to be a stub that
// never refreshed: an imported token silently expired while the ug check-in
// still "passed" through its !enable branch, and chat failed with code=1001
// on every attempt (ticket 37). Leaves creds untouched on any failure so the
// old refresh token stays retryable.
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	// Backfill the device identity for accounts imported before BP-98 TRAE-2
	// (their blobs carry device_id:"" / machine_id:""): derive them from the
	// uid once, mark dirty, and let the refresh task persist. Without a
	// device the check-in reads as a script and is answered 9074.
	if err := backfillTraeDevice(acc); err != nil {
		return err
	}
	return d.refreshToken(ctx, acc, false)
}

// backfillTraeDevice fills an empty device/machine pair in the credential
// blob with a per-account stable one. Deterministic on (device_id, uid);
// for a blob with neither, uid alone seeds it, so it never rotates.
func backfillTraeDevice(acc *model.Account) error {
	if acc == nil {
		return nil
	}
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return nil // let the caller's own parse report the real problem
	}
	if creds.DeviceID != "" || creds.UID == "" {
		return nil
	}
	creds.DeviceID = ugDeviceID("", creds.UID)
	creds.MachineID = ugDeviceID("", creds.UID+"-machine")
	raw, err := json.Marshal(creds)
	if err != nil {
		return nil
	}
	acc.SetCredentials(raw)
	acc.CredsDirty = true
	log.Printf("trae: backfilled device identity for account %s (BP-98 TRAE-2)", acc.ID)
	return nil
}

// refreshToken is the shared ExchangeToken core. force=true skips the expiry
// skew (used by Stream's self-heal after an auth rejection: the stored expiry
// can look fine while the JWT has been superseded upstream). Concurrent
// callers on one account share a single exchange (refreshInFlight): the
// refresh token is one-time, and racing it turned the loser's 20101 into a
// disabled account.
func (d *Driver) refreshToken(ctx context.Context, acc *model.Account, force bool) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return err
	}
	if !force && creds.ExpiresAt != 0 && time.Until(time.Unix(creds.ExpiresAt, 0)) > traeRefreshSkew {
		return nil // still fresh — nothing to do
	}
	// Single-flight gate: the first caller for this account does the exchange;
	// the others wait for it and then re-check (a completed rotation makes
	// their creds fresh, so they return without touching upstream).
	if acc != nil && acc.ID != "" {
		for {
			gate, loaded := refreshInFlight.LoadOrStore(acc.ID, make(chan struct{}))
			done := gate.(chan struct{})
			if !loaded {
				defer func() {
					close(done)
					refreshInFlight.Delete(acc.ID)
				}()
				break
			}
			select {
			case <-done:
				nc, perr := ParseCreds(acc.Credentials())
				if perr == nil && !force && (nc.ExpiresAt == 0 || time.Until(time.Unix(nc.ExpiresAt, 0)) <= traeRefreshSkew) {
					// Still stale after the winner's exchange (it may have
					// failed): fall through and retry the gate once more.
					continue
				}
				return nil
			case <-ctx.Done():
				return uperr.New(uperr.ClassCanceled, 0, "trae refresh: %v", ctx.Err())
			}
		}
	}
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return uperr.New(uperr.ClassAuth, 401, "trae: no refresh token (re-login required)")
	}
	body := map[string]any{
		"ClientID":     traeOAuthClient,
		"RefreshToken": creds.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, traeOAuthHost+traeEpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+ideVersion)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae refresh: %v", err)
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return uperr.New(uperr.ClassTransport, 0, "trae refresh read: %v", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		// 20101 "refresh token is invalid": the stored credential lineage is
		// dead — chat 1001s while ug endpoints keep answering. Only a fresh
		// browser login fixes it (ticket 37 verified live 2026-10-16).
		if strings.Contains(string(data), "refresh token is invalid") || strings.Contains(string(data), "20101") {
			return uperr.New(uperr.ClassAuth, 401,
				"trae: refresh token invalid — re-login required (panel → Trae 登录): %s", truncate(data, 160))
		}
		return uperr.New(uperr.ClassAuth, resp.StatusCode, "trae refresh: status %d: %s", resp.StatusCode, truncate(data, 200))
	}
	var out struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return uperr.New(uperr.ClassInternal, 0, "trae refresh parse: %v", err)
	}
	if out.Result.Token == "" {
		return uperr.New(uperr.ClassAuth, 401, "trae: refresh rejected — re-login required")
	}
	creds.AccessToken = out.Result.Token
	if out.Result.RefreshToken != "" {
		creds.RefreshToken = out.Result.RefreshToken
	}
	switch {
	case out.Result.TokenExpireAt > 0:
		creds.ExpiresAt = normalizeTraeExpire(out.Result.TokenExpireAt)
	case out.Result.TokenExpireDuration > 0:
		creds.ExpiresAt = time.Now().Add(time.Duration(out.Result.TokenExpireDuration) * time.Second).Unix()
	}
	rawCreds, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	acc.SetCredentials(rawCreds) // persisted by the refresh task / gateway after success
	return nil
}

// normalizeTraeExpire converts the upstream's millisecond TokenExpireAt to
// Unix seconds (1e12 distinguishes ms from s — reference parity).
func normalizeTraeExpire(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
