// Package zcode implements the Z.AI ZCode channel: IR-in, Anthropic-shaped
// upstream-out via the start-plan endpoint, with the official client identity
// blocks (content inspection rejects requests without them — error 3012),
// model aliasing, per-(account, model) quota death marking and JWT auth.
//
// The identity block asset in assets/ is ported from the MIT-licensed
// workbuddy2api-panel fork (see NOTICE); the upstream requires the official
// client identity to pass content inspection.
package zcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

//go:embed assets/zcode_system.json
var embeddedSystem []byte

// Creds is the persisted credential shape for a zcode account.
type Creds struct {
	JWT           string `json:"jwt"` // Bearer token; the JWT sub is the user id
	DeviceMID     string `json:"device_mid,omitempty"`
	Captcha       string `json:"captcha,omitempty"`
	CaptchaRegion string `json:"captcha_region,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
}

// JWTSub extracts the subject (user id) from a JWT without verification
// (payload is base64url JSON; verification happens upstream-side).
func JWTSub(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate padded variants.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Sub    string `json:"sub"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.Sub != "" {
		return claims.Sub
	}
	return claims.UserID
}

// modelNameMap maps client aliases to the upstream billing ids. Names the
// origin never serves (lowercase glm-*, claude-* probes) must resolve here.
var modelNameMap = map[string]string{
	"glm-5.3":                 "GLM-5.3",
	"glm-5.3-flash":           "GLM-5.3-Flash",
	"glm-5.2":                 "GLM-5.2",
	"glm-5-turbo":             "GLM-5-Turbo",
	"glm-turbo":               "GLM-5-Turbo",
	"glm-5.1":                 "GLM-5.1",
	"glm-4.7":                 "GLM-4.7",
	"claude-opus-4-8":         "GLM-5.3",
	"claude-sonnet-4-6":       "GLM-5.3",
	"claude-3-5-haiku-latest": "GLM-5.3-Flash",
}

// ResolveModel normalizes a client model name to the upstream id.
func ResolveModel(name string) string {
	if mapped, ok := modelNameMap[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return name
}

// Driver implements driver.Driver for the zcode channel.
type Driver struct {
	cfg  config.ZCodeSettings
	http *http.Client // streaming chat calls (no total timeout + header/idle watchdogs)
	rpc        *http.Client // short probe / plan calls (total timeout)
	giftClient *http.Client // billing-plane gift calls (30s, BP-231a)
	// allowUnsignedThinking gates replay of history thinking blocks whose
	// upstream signature is empty. If the upstream ever rejects such a replay,
	// one 400 is enough to disable it for this process — otherwise every tool
	// turn would pay an extra round trip (BP-69).
	allowUnsignedThinking atomic.Bool
}

// New builds the driver.
func New(cfg config.ZCodeSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	rpc := httpx.RPCClient()
	// BP-231a: an explicit per-channel proxy (Clash HTTP port) replaces the
	// clients' default transport — the dial AND the name resolution then
	// happen at the proxy, bypassing this process's resolver (which on a
	// TUN/fake-ip setup was observed hanging for minutes on zcode.z.ai
	// while short-lived processes resolved instantly).
	var tr http.RoundTripper
	if cfg.Proxy != "" {
		if pu, perr := url.Parse(cfg.Proxy); perr == nil && pu.Scheme == "http" {
			pt := http.DefaultTransport.(*http.Transport).Clone()
			pt.Proxy = http.ProxyURL(pu)
			tr = pt
		} else {
			log.Printf("zcode: proxy %q ignored (must be an http:// URL)", cfg.Proxy)
		}
	}
	d := &Driver{cfg: cfg, http: httpClient, rpc: rpc}
	if tr != nil {
		httpClient = &http.Client{Transport: tr} // streaming chat: no total timeout
		rpc = &http.Client{Transport: tr, Timeout: httpx.RPCTimeout()}
		d.http, d.rpc = httpClient, rpc
	}
	// giftClient is the billing plane's client (BP-231): a gift claim must
	// never hold the task for the full rpc budget — 30s and the verdict is a
	// retryable failure.
	d.giftClient = &http.Client{Timeout: giftCallTimeout}
	if tr != nil {
		d.giftClient = &http.Client{Transport: tr, Timeout: giftCallTimeout}
	}
	d.allowUnsignedThinking.Store(true)
	return d
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelZCode }

// ParseCreds decodes a credential blob.
func ParseCreds(raw []byte) (*Creds, error) {
	var c Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("zcode: parse creds: %w", err)
	}
	return &c, nil
}

func (d *Driver) base() string { return strings.TrimRight(d.cfg.BaseURL, "/") }

// messagesPath is the start-plan Anthropic-shaped endpoint.
const messagesPath = "/api/v1/zcode-plan/anthropic/v1/messages"

// identityHeaders is the official-client header set (order-insensitive).
// sessionID is the conversation's bare-UUID x-session-id: BigModel keeps the
// prompt cache per (session, device) and seldom for requests that name none
// (magpie zcode #388 — hit rate went 1/19 → 13/16 once it was sent).
func identityHeaders(req *http.Request, c *Creds, appVersion, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.JWT)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai/")
	req.Header.Set("User-Agent", "ZCode/"+appVersion)
	req.Header.Set("X-ZCode-App-Version", appVersion)
	req.Header.Set("X-Title", "Z Code@cli")
	req.Header.Set("X-ZCode-Agent", "glm")
	req.Header.Set("X-Platform", "win32-x64")
	req.Header.Set("X-Release-Channel", "production")
	req.Header.Set("X-Client-Language", "en-001")
	req.Header.Set("X-Os-Category", "windows")
	req.Header.Set("X-Os-Version", "10.0.26200")
	if c.DeviceMID != "" {
		req.Header.Set("X-Device-Mid", c.DeviceMID)
	}
	req.Header.Set("x-zcode-session-type", "main")
	req.Header.Set("x-session-id", sessionID)
	if c.Captcha != "" {
		req.Header.Set("X-Aliyun-Captcha-Verify-Param", c.Captcha)
		if c.CaptchaRegion != "" {
			req.Header.Set("X-Aliyun-Captcha-Verify-Region", c.CaptchaRegion)
		}
	}
	// x-request-id / x-zcode-trace-id are regenerated per request.
	req.Header.Set("x-request-id", newUUID())
	req.Header.Set("x-zcode-trace-id", newUUID())
}

const defaultAppVersion = "1.0.7"

// Stream implements driver.Driver: IR -> Anthropic body -> start-plan
// endpoint -> Anthropic SSE -> IR events.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	creds, err := ParseCreds(call.Account.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	if creds.JWT == "" {
		return nil, uperr.New(uperr.ClassAuth, 401, "zcode: missing jwt")
	}
	body, hadThinking, hadUnsigned, err := d.normalizeBody(call, creds, true)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "zcode: build body: %v", err)
	}
	sessionID := sessionIDFor(creds, accountIDOf(call), call.Request.SessionKey)
	do := func(wire []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base()+messagesPath, bytes.NewReader(wire))
		if err != nil {
			return nil, err
		}
		identityHeaders(req, creds, defaultAppVersion, sessionID)
		return d.http.Do(req)
	}
	resp, err := do(body)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "zcode: %v", err)
	}
	if resp.StatusCode == http.StatusBadRequest && hadThinking {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if thinkingReplayRejected(raw) {
			// The replayed thinking block was rejected (foreign/stale
			// signature). Degrade to the pre-BP-69 body — dropping history
			// thinking is safe, so the request must not fail because we tried
			// to preserve it.
			if hadUnsigned {
				d.allowUnsignedThinking.Store(false)
				log.Printf("zcode: unsigned history thinking rejected; disabling unsigned replay for this process")
			}
			log.Printf("zcode: history thinking rejected by upstream; retrying without it")
			degraded, _, _, berr := d.normalizeBody(call, creds, false)
			if berr != nil {
				return nil, uperr.New(uperr.ClassInternal, 0, "zcode: rebuild body: %v", berr)
			}
			resp, err = do(degraded)
			if err != nil {
				return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "zcode: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				return nil, classifyHTTP(resp.StatusCode, raw, call.UpstreamModel)
			}
		} else {
			return nil, classifyHTTP(http.StatusBadRequest, raw, call.UpstreamModel)
		}
	} else if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw, call.UpstreamModel)
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return d.parseAnthropicSSE(ctx, streamBody, call, emit)
}

// thinkingReplayRejected recognizes the Anthropic 400 family that complains
// about a replayed thinking/signature block.
func thinkingReplayRejected(raw []byte) bool {
	s := strings.ToLower(string(raw))
	return strings.Contains(s, "thinking") || strings.Contains(s, "signature")
}

// normalizeBody converts the IR request into the Anthropic body expected by
// the start-plan endpoint, applying: model alias resolution, official
// identity blocks + dynamic currentModel block, user system appended after,
// cache_control ephemeral on the last non-system block, metadata.user_id =
// JWT sub, and (BP-69) replayable assistant thinking blocks, tool_choice and
// top_k. withThinkingHistory=false builds the degraded body used when the
// upstream rejects replayed thinking.
func (d *Driver) normalizeBody(call driver.ChatCall, creds *Creds, withThinkingHistory bool) ([]byte, bool, bool, error) {
	req := call.Request
	upstreamModel := ResolveModel(call.UpstreamModel)
	if call.UpstreamModel != "" {
		upstreamModel = ResolveModel(call.UpstreamModel)
	} else {
		upstreamModel = ResolveModel(req.Model)
	}

	var messages []map[string]any
	var userSystem []string
	hadThinking := false
	hadUnsigned := false
	for _, m := range req.Messages {
		if m.Role == ir.RoleSystem {
			for _, p := range m.Content {
				if p.Kind == "text" && p.Text != "" {
					userSystem = append(userSystem, p.Text)
				}
			}
			continue
		}
		wire := map[string]any{"role": string(m.Role)}
		var parts []map[string]any
		// Replay signed/unsigned thinking blocks for assistant tool-use turns
		// (Anthropic requires the block immediately preceding tool results).
		// They must come before text and tool_use. Plain assistant turns are
		// not replayed: dropping them is safe and saves prompt tokens.
		if withThinkingHistory && m.Role == ir.RoleAssistant && len(m.ToolCalls) > 0 {
			for _, th := range m.Thinking {
				if th.Text == "" && th.Signature == "" {
					continue
				}
				if th.Signature == "" && !d.allowUnsignedThinking.Load() {
					continue
				}
				parts = append(parts, map[string]any{
					"type": "thinking", "thinking": th.Text, "signature": th.Signature,
				})
				hadThinking = true
				if th.Signature == "" {
					hadUnsigned = true
				}
			}
		}
		for _, p := range m.Content {
			switch p.Kind {
			case "text":
				if p.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			case "image":
				parts = append(parts, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type": "base64", "media_type": p.ImageMIME, "data": p.ImageB64,
					},
				})
			}
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
					input = map[string]any{}
				}
				parts = append(parts, map[string]any{"type": "tool_use", "id": tc.ID, "name": toolOutName(tc.Name), "input": input})
			}
		}
		if m.Role == ir.RoleTool {
			text := ir.TextOf(m.Content)
			var blocks []map[string]any
			hasImage := false
			for _, p := range m.Content {
				switch p.Kind {
				case "text":
					if p.Text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
					}
				case "image":
					hasImage = true
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "base64", "media_type": p.ImageMIME, "data": p.ImageB64,
						},
					})
				}
			}
			var result any = text
			if hasImage {
				result = blocks
			}
			wire = map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": m.ToolCallID, "content": result,
				}},
			}
			messages = append(messages, wire)
			continue
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"type": "text", "text": ""})
		}
		wire["content"] = parts
		messages = append(messages, wire)
	}

	// Official identity blocks + dynamic text merged into the LAST block.
	// zcode.z.ai rejects a fourth system block with 405/3012 (magpie #343),
	// and ZCode's own client joins every dynamic section into the third
	// block's text with "\n\n" — so the model declaration and the caller's
	// system prompts must never become blocks of their own.
	var system []map[string]any
	var official []map[string]any
	if err := json.Unmarshal(embeddedSystem, &official); err == nil && len(official) > 0 {
		system = append(system, official...)
	}
	var dynamic []string
	if strings.TrimSpace(upstreamModel) != "" {
		dynamic = append(dynamic, "- You are powered by the model named "+upstreamModel+".")
	}
	for _, us := range userSystem {
		if strings.TrimSpace(us) != "" {
			dynamic = append(dynamic, us)
		}
	}
	if len(dynamic) > 0 {
		if len(system) > 0 {
			last := system[len(system)-1]
			prev, _ := last["text"].(string)
			last["text"] = prev + "\n\n" + strings.Join(dynamic, "\n\n")
			if _, ok := last["cache_control"]; !ok {
				last["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		} else {
			// Embedded asset missing: one merged block, still within the cap.
			system = append(system, map[string]any{
				"type":          "text",
				"text":          strings.Join(dynamic, "\n\n"),
				"cache_control": map[string]any{"type": "ephemeral"},
			})
		}
	}

	// cache_control ephemeral on the last content block of the last
	// non-system message.
	for i := len(messages) - 1; i >= 0; i-- {
		if content, ok := messages[i]["content"].([]map[string]any); ok && len(content) > 0 {
			content[len(content)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
			break
		}
	}

	body := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"system":   system,
		"stream":   true, // forced; non-stream aggregated locally
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	} else {
		body["max_tokens"] = 8192
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		body["top_k"] = *req.TopK
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tool := map[string]any{"name": toolOutName(t.Name)}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			if t.Parameters != nil {
				tool["input_schema"] = t.Parameters
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		thinking := map[string]any{"type": "enabled"}
		if req.Thinking.BudgetTokens > 0 {
			thinking["budget_tokens"] = req.Thinking.BudgetTokens
		}
		body["thinking"] = thinking
	} else if budget := ir.BudgetFromLevel(req.RequestedEffort); budget > 0 {
		// Chat/Responses ingress carries reasoning_effort only. Without this
		// fallback the zcode channel silently ran without thinking whenever
		// the client did not send an Anthropic thinking block (BP-69).
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	if tc := anthropicToolChoice(req.ToolChoice, toolOutName(req.ToolChoiceName), req.DisableParallel); tc != nil {
		body["tool_choice"] = tc
	}
	// metadata.user_id as the official client puts it: a JSON STRING naming
	// the device, no account uuid, and the request's x-session-id (magpie
	// zcode 0.1.30 / #387 — the plan endpoint keeps its prompt cache per
	// machine and mostly misses without it).
	if device := deviceIDFor(creds, accountIDOf(call)); device != "" {
		userID, _ := json.Marshal(map[string]any{
			"device_id": device, "account_uuid": "", "session_id": sessionIDFor(creds, accountIDOf(call), req.SessionKey),
		})
		body["metadata"] = map[string]any{"user_id": string(userID)}
	}
	raw, err := json.Marshal(body)
	return raw, hadThinking, hadUnsigned, err
}

// anthropicToolChoice renders the IR's portable choice vocabulary on the
// Anthropic wire. The zcode driver previously sent no tool_choice at all, so
// forced/named tool calls were silently ignored on this channel (BP-69).
func anthropicToolChoice(choice any, name string, disableParallel bool) map[string]any {
	var out map[string]any
	switch c, _ := choice.(string); c {
	case "required":
		if name != "" {
			out = map[string]any{"type": "tool", "name": name}
		} else {
			out = map[string]any{"type": "any"}
		}
	case "auto":
		out = map[string]any{"type": "auto"}
	case "none":
		out = map[string]any{"type": "none"}
	default:
		if !disableParallel {
			return nil
		}
		out = map[string]any{"type": "auto"}
	}
	if disableParallel {
		out["disable_parallel_tool_use"] = true
	}
	return out
}

// parseAnthropicSSE consumes the Anthropic-shaped upstream stream.
func (d *Driver) parseAnthropicSSE(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: time.Now().Unix(), Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		thinkSig   string
		tools      = map[int]*ir.ToolCall{}
		toolOrder  []int
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "zcode: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// Quota family errors may arrive mid-stream as JSON bodies.
		if code, msg, ok := detectBusinessError([]byte(payload)); ok && code != 0 {
			return nil, classifyBusiness(code, msg, call.UpstreamModel)
		}
		var ev struct {
			Type string `json:"type"`
			// message_start
			Message *struct {
				Usage *struct {
					InputTokens int `json:"input_tokens"`
					// Anthropic-wire prefix cache accounting (BP-53). Pointers:
					// absent must not read as a reported zero.
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			// content_block_start
			Index        int `json:"index"`
			ContentBlock *struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Signature string `json:"signature"`
			} `json:"content_block"`
			// content_block_delta
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			// message_delta
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				// ir.Usage keeps one convention for every channel: InputTokens
				// is the FULL prompt, cache included (workbuddy/antigravity
				// report it that way). This upstream speaks Anthropic, whose
				// input_tokens excludes the cache split — fold it back into the
				// total, or every downstream renderer under-reports (BP-56).
				total := u.InputTokens
				if u.CacheCreationInputTokens != nil {
					total += *u.CacheCreationInputTokens
				}
				if u.CacheReadInputTokens != nil {
					total += *u.CacheReadInputTokens
					usage.CachedInputTokens = *u.CacheReadInputTokens
					usage.CacheKnown = true
				}
				usage.InputTokens = total
				gotUsage = true
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "thinking" && ev.ContentBlock.Signature != "" {
				thinkSig = ev.ContentBlock.Signature
				if err := emit(ir.StreamEvent{Kind: ir.EvThinkingSignature, Delta: thinkSig}); err != nil {
					return nil, err
				}
			}
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				tc := &ir.ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
				tools[ev.Index] = tc
				toolOrder = append(toolOrder, ev.Index)
				if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: ev.Index, ToolCall: &ir.ToolCall{ID: tc.ID, Name: toolInName(tc.Name)}}); err != nil {
					return nil, err
				}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				textBuf.WriteString(ev.Delta.Text)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ev.Delta.Text}); err != nil {
					return nil, err
				}
			case "thinking_delta":
				thinkBuf.WriteString(ev.Delta.Thinking)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ev.Delta.Thinking}); err != nil {
					return nil, err
				}
			case "signature_delta":
				thinkSig = ev.Delta.Signature
				if thinkSig != "" {
					if err := emit(ir.StreamEvent{Kind: ir.EvThinkingSignature, Delta: thinkSig}); err != nil {
						return nil, err
					}
				}
			case "input_json_delta":
				if tc, ok := tools[ev.Index]; ok {
					tc.Arguments += ev.Delta.PartialJSON
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: ev.Index, Delta: ev.Delta.PartialJSON}); err != nil {
						return nil, err
					}
				}
			}
		case "message_delta":
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
				gotUsage = true
			}
		case "message_stop":
			terminated = true
			stop = ir.StopEnd
		case "error":
			// error events carry the same business codes.
			if code, msg, ok := detectBusinessError([]byte(payload)); ok {
				return nil, classifyBusiness(code, msg, call.UpstreamModel)
			}
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"zcode: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "zcode: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "zcode: stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "zcode: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "zcode: stream ended without message_stop")
	}
	// An exhausted plan / rejected model can end the SSE stream with no text,
	// no tool call and no usage. Treat that as a failure so the gateway moves
	// to the next binding instead of returning an empty 200.
	if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolOrder) == 0 && !gotUsage {
		return nil, uperr.New(uperr.ClassTransport, 0,
			"zcode: empty completion (quota exhausted or model unavailable)")
	}
	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *tools[idx])
	}
	if len(out.Message.ToolCalls) > 0 {
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.ThinkingSignature = thinkSig
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

// quotaFamilyCodes are the upstream 429-family model-exhaustion codes.
var quotaFamilyCodes = map[int]bool{1113: true, 1308: true, 1310: true, 1311: true, 1312: true, 1313: true, 1314: true, 1315: true, 1316: true, 1317: true, 1318: true, 1319: true, 1320: true, 1321: true}

func detectBusinessError(raw []byte) (int, string, bool) {
	var doc struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, "", false
	}
	code := toInt(doc.Code)
	msg := doc.Message
	if code == 0 && doc.Error != nil {
		code = toInt(doc.Error.Code)
		msg = doc.Error.Message
	}
	if code == 0 {
		return 0, "", false
	}
	return code, msg, true
}

func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n := 0
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	default:
		return 0
	}
}

func classifyBusiness(code int, msg, upstreamModel string) *uperr.Error {
	switch {
	case code == 1005 || quotaFamilyCodes[code]:
		e := uperr.New(uperr.ClassSoftRate, 200, "zcode: quota exhausted: %s", msg)
		e.Model = ResolveModel(upstreamModel) // model-scoped: other models stay live
		e.ResetAt = time.Now().Add(6 * time.Hour)
		return e
	case code == 3012:
		// 405 {"code":3012,"msg":"request has been blocked due to unusual
		// activity."} is upstream risk control on the ACCOUNT (magpie zcode
		// 0.1.27: failRestricted), not a property of the payload — the same
		// body passes on a healthy account. Terminal here, matching the
		// 11140 discipline: the user re-enables after a manual look.
		return uperr.New(uperr.ClassAccountRestricted, 405, "zcode: upstream risk control (3012): %s", msg)
	case code == 3008 || code == 3009:
		// BP-148: 3008 "user concurrency limit exceeded" / 3009 "model
		// concurrency limit exceeded" arrive on a 429 when one account has more
		// in flight than its plan serves (magpie zcode 0.1.32 / #395). That is
		// throughput backpressure, not an exhausted plan: the account is
		// healthy and will answer once a slot frees. Classifying it as
		// ClassInternal (or letting the bare 429 branch give it a 1-hour
		// ResetAt) took a working account out of service for an hour over a
		// sub-second collision.
		//
		// ClassUpstreamBusy is the pool's "wait, don't switch" class: no account
		// penalty, and a short channel+model pause so follow-up requests do not
		// stampede the same full concurrency window.
		e := uperr.New(uperr.ClassUpstreamBusy, 429, "zcode: concurrency limit (code=%d, transient): %s", code, msg)
		e.Code = fmt.Sprint(code)
		e.Model = ResolveModel(upstreamModel)
		return e
	case code == 401 || code == 403:
		return uperr.New(uperr.ClassAuth, code, "zcode: %s", msg)
	default:
		return uperr.New(uperr.ClassInternal, 200, "zcode: code=%d %s", code, msg)
	}
}

func classifyHTTP(status int, body []byte, upstreamModel string) *uperr.Error {
	if code, msg, ok := detectBusinessError(body); ok {
		return classifyBusiness(code, msg, upstreamModel)
	}
	switch {
	case status == 401:
		return uperr.New(uperr.ClassAuth, status, "zcode: %s", truncate(body, 300))
	case status == 429:
		e := uperr.New(uperr.ClassSoftRate, status, "zcode: %s", truncate(body, 300))
		e.ResetAt = time.Now().Add(1 * time.Hour)
		return e
	case status == 403:
		if uperr.IsEdgeBlock(body) {
			return uperr.EdgeBlocked(status, "zcode: edge blocked: %s", truncate(body, 300))
		}
		return uperr.New(uperr.ClassWAF, status, "zcode: %s", truncate(body, 300))
	case status == 400:
		return uperr.New(uperr.ClassBadRequest, status, "zcode: %s", truncate(body, 300))
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "zcode: %s", truncate(body, 300))
	default:
		return uperr.New(uperr.ClassInternal, status, "zcode: %s", truncate(body, 300))
	}
}

// Probe implements driver.Prober: fetch quota via the zcode-plan endpoint
// (best effort; success only means the JWT is alive).
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	if creds.JWT == "" {
		return nil, uperr.New(uperr.ClassAuth, 401, "zcode: missing jwt")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base()+"/api/v1/zcode-plan", nil)
	if err != nil {
		return nil, err
	}
	identityHeaders(req, creds, defaultAppVersion, bareUUID(deviceIDFor(creds, acc.ID)))
	req.Header.Set("Accept", "application/json")
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "zcode probe: %v", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 {
		return nil, uperr.New(uperr.ClassAuth, 401, "zcode: jwt rejected")
	}
	if resp.StatusCode != http.StatusOK {
		return &driver.ProbeResult{Note: fmt.Sprintf("zcode plan status %d", resp.StatusCode)}, nil
	}
	// The plan body feeds the credit reading; a truncated read must not be
	// silently treated as "no credits field present" (ticket 11).
	if rerr != nil {
		log.Printf("zcode: plan probe read failed (account=%s, endpoint=/api/v1/zcode-plan): %v", acc.ID, rerr)
		return nil, uperr.New(uperr.ClassTransport, 0, "zcode probe: read body: %v", rerr)
	}
	// Best-effort parse of a credits field if present.
	var doc struct {
		Data struct {
			Remain  int64 `json:"remain"`
			Credits int64 `json:"credits"`
			Total   int64 `json:"total"`
		} `json:"data"`
		Remain  int64 `json:"remain"`
		Credits int64 `json:"credits"`
	}
	_ = json.Unmarshal(raw, &doc)
	remain := doc.Data.Remain
	if remain == 0 {
		remain = doc.Data.Credits
	}
	if remain == 0 {
		remain = doc.Remain
	}
	if remain == 0 {
		remain = doc.Credits
	}
	res := &driver.ProbeResult{Note: "zcode jwt alive"}
	if remain > 0 {
		c := model.Credits{Remain: remain, Total: doc.Data.Total, UpdatedAt: time.Now()}
		res.Credits = &c
	}
	return res, nil
}

// Models returns the curated model list.
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return []string{
		"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5-turbo", "glm-5.1", "glm-4.7",
		"claude-opus-4-8", "claude-sonnet-4-6", "claude-3-5-haiku-latest",
	}, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// accountIDOf is a nil-safe read of the account id.
func accountIDOf(call driver.ChatCall) string {
	if call.Account != nil {
		return call.Account.ID
	}
	return ""
}

// ToolSearch rename: Claude Code 2.1+ subagents carry a tool named
// ToolSearch, and the endpoint hides Bash/Read/Write once a tool with that
// exact name is declared (magpie zcode 0.1.24). Outbound it goes as
// ToolSearchTool; the model's calls come back under the alias and are
// restored before they reach the client.
const (
	toolSearchName  = "ToolSearch"
	toolSearchAlias = "ToolSearchTool"
)

func toolOutName(name string) string {
	if name == toolSearchName {
		return toolSearchAlias
	}
	return name
}

func toolInName(name string) string {
	if name == toolSearchAlias {
		return toolSearchName
	}
	return name
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// bareUUID renders a conversation id as the bare UUID the official client
// sends in x-session-id: an already-UUID key goes as it is, anything else
// becomes a stable UUID derived from it (magpie zcode sessionOf). BigModel
// keeps a prompt's cache for requests naming the same session and seldom for
// those naming none.
func bareUUID(seed string) string {
	s := strings.ToLower(strings.TrimSpace(seed))
	if uuidRe.MatchString(s) {
		return s
	}
	sum := sha256.Sum256([]byte("zcode-session:" + s))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// deviceIDFor names the machine the way the official client's metadata does:
// the sign-in's device id when we have one, else a stable UUID-shaped stand-in
// derived from the account identity.
func deviceIDFor(creds *Creds, accountID string) string {
	if creds != nil && creds.DeviceMID != "" {
		return creds.DeviceMID
	}
	if creds != nil {
		if sub := JWTSub(creds.JWT); sub != "" {
			return bareUUID(sub)
		}
	}
	return bareUUID(accountID)
}

// sessionIDFor is the request's x-session-id: the ingress session key when
// there is one, else a per-device stable id (magpie zcode sessionOf).
func sessionIDFor(creds *Creds, accountID, sessionKey string) string {
	if k := strings.TrimSpace(sessionKey); k != "" {
		return bareUUID(k)
	}
	return bareUUID(deviceIDFor(creds, accountID))
}
