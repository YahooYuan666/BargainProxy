// Package trae implements the Trae CN (SOLO) channel: OpenAI-in,
// SOLO-llm_utils_chat-out with forced streaming, serial per-account
// concurrency (official free-tier limit), daily checkin and credit queries.
//
// Behaviour notes (clean-room from research/2026-10-13-三项目与生态调研.md):
// the upstream chat endpoint is /api/agent/v3/llm_utils_chat on the SOLO
// agent host with a fixed header set; config_name injection breaks newer
// models so only `model` is sent; error 1005 means plan entitlement
// exhaustion (hard cooldown), 401 means the IDE JWT died.
package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

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
)

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

// ugHeaders sets the checkin/usage header set.
func ugHeaders(req *http.Request, c *Creds) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+ideVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	if c.DeviceID != "" {
		req.Header.Set("X-Device-Id", c.DeviceID)
	}
}

// Stream implements driver.Driver: OpenAI chat body -> SOLO body, forced
// stream, upstream SSE parsed into IR events.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	creds, err := ParseCreds(call.Account.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	body, err := buildSOLOBody(call)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "trae: build body: %v", err)
	}
	resp, err := d.chatOnce(ctx, creds, body, call, emit)
	if err == nil {
		return resp, nil
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
			if nc, perr := ParseCreds(call.Account.CredsJSON); perr == nil {
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
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return parseSSE(ctx, streamBody, call, emit)
}

// buildSOLOBody converts the IR request into the SOLO chat body:
// {messages, function:"solo_work_lite", stream:true, model}.
func buildSOLOBody(call driver.ChatCall) ([]byte, error) {
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
				if p.Kind == "text" {
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			}
			wire["content"] = parts
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
		"function": functionSOLO,
		"stream":   true, // forced
		"model":    call.UpstreamModel,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
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
		textBuf   strings.Builder
		thinkBuf  strings.Builder
		usage     ir.Usage
		stop      ir.StopReason
		gotUsage  bool
		toolAcc   = map[int]*ir.ToolCall{}
		toolOrder []int
		started   = map[int]bool{}
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
				if v, ok := fn["arguments"].(string); ok {
					frag = v
					acc.Arguments += v
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
			var u struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				ReasoningTokens  int `json:"reasoning_tokens"`
			}
			if err := json.Unmarshal([]byte(payload), &u); err != nil {
				return nil
			}
			usage = ir.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, ReasoningTokens: u.ReasoningTokens}
			gotUsage = true
			return nil
		case "done":
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

func detectError(payload string) (int, string, bool) {
	var doc struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return 0, "", false
	}
	code := 0
	switch t := doc.Code.(type) {
	case float64:
		code = int(t)
	case string:
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0, "", false
			}
			code = code*10 + int(c-'0')
		}
	}
	if code == 0 {
		return 0, "", false
	}
	return code, doc.Message, true
}

func classifyBusiness(code int, msg string) *uperr.Error {
	switch code {
	case 1001:
		// "We're sorry, but we are not able to authenticate you": the JWT was
		// rejected (stale/rotated). ClassAuth so Stream can self-heal with a
		// forced token exchange (ticket 37).
		return uperr.New(uperr.ClassAuth, 401, "trae: %s", msg)
	case 1005:
		e := uperr.New(uperr.ClassSoftRate, 200, "trae: plan entitlement exhausted: %s", msg)
		e.ResetAt = time.Now().Add(12 * time.Hour)
		return e
	case 4001:
		// Upstream rejected the payload shape ("bad request: json: cannot
		// unmarshal …"): the account is fine, the body was not — no penalty,
		// no rotation (mirrors workbuddy's 11101).
		return uperr.New(uperr.ClassBadRequest, 400, "trae: %s", msg)
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
		e := uperr.New(uperr.ClassModelRate, status, "trae: %s", truncate(body, 300))
		e.ResetAt = time.Now().Add(60 * time.Second)
		return e
	case status == 403:
		return uperr.New(uperr.ClassWAF, status, "trae: %s", truncate(body, 300))
	case status == 400:
		return uperr.New(uperr.ClassBadRequest, status, "trae: %s", truncate(body, 300))
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "trae: %s", truncate(body, 300))
	default:
		return uperr.New(uperr.ClassInternal, status, "trae: %s", truncate(body, 300))
	}
}

// Checkin performs the daily checkin (idempotent upstream-side).
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) (checkedIn bool, credits int64, enable bool, err error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return false, 0, false, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	// Status first.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ugBase()+"/trae/api/v2/ug/checkin_credits/status", bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	ugHeaders(req, creds)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return false, 0, false, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae checkin status: %v", err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, 0, false, classifyHTTP(resp.StatusCode, raw)
	}
	// The status/credits below feed task decisions; a truncated read must not
	// read as "not checked in, zero credits" (ticket 11).
	if rerr != nil {
		log.Printf("trae: checkin status read failed (account=%s, endpoint=checkin_credits/status): %v", acc.ID, rerr)
		return false, 0, false, uperr.New(uperr.ClassTransport, 0, "trae checkin status: read body: %v", rerr)
	}
	var st struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	_ = json.Unmarshal(raw, &st)
	if st.CheckedIn || !st.Enable {
		// enable=false means the check-in program is unavailable for this
		// account — reporting it as "already checked in" masked auth failures
		// (ticket 37); callers now see the distinction.
		return st.CheckedIn, st.Credits, st.Enable, nil
	}
	// Claim. Upstream may answer HTTP 200 with a non-zero business code (e.g.
	// 9074 "当前参与用户太多"): treat it as a failed claim and retry briefly
	// before surfacing the error (agentpoints2 parity).
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		lastErr = d.claimCheckin(ctx, creds)
		if lastErr == nil {
			return true, st.Credits, true, nil
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return false, 0, false, uperr.New(uperr.ClassCanceled, 0, "trae checkin: %v", ctx.Err())
			case <-time.After(checkinRetryDelay):
			}
		}
	}
	return false, 0, false, lastErr
}

// checkinRetryDelay is a package var so tests can shorten the backoff.
var checkinRetryDelay = 20 * time.Second

// claimCheckin performs one claim attempt and verifies the business code.
func (d *Driver) claimCheckin(ctx context.Context, creds *Creds) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ugBase()+"/trae/api/v2/ug/checkin_credits/claim", bytes.NewReader([]byte("{}")))
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
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		return uperr.New(uperr.ClassTransport, 0, "trae checkin claim code=%d: %s", env.Code, env.Message)
	}
	return nil
}

// Probe implements driver.Prober: fetch credit balance.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	creds, err := ParseCreds(acc.CredsJSON)
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
	remain, limit, _, packs, err := ParseEntUsage(raw)
	if err != nil {
		return nil, uperr.New(uperr.ClassInternal, 0, "trae usage parse: %v", err)
	}
	_ = packs
	credits := &model.Credits{Remain: remain, Total: limit, UpdatedAt: time.Now()}
	return &driver.ProbeResult{Credits: credits, Note: "trae credits refreshed"}, nil
}

// ParseEntUsage aggregates the entitlement pack list: remain = limit - used.
func ParseEntUsage(raw []byte) (remain, limit, used int64, packs int, err error) {
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, 0, 0, 0, err
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs++
	}
	if remain < 0 {
		remain = 0
	}
	return remain, limit, used, packs, nil
}

// Models returns the curated model list (dynamic discovery uses
// get_detail_param during probes; the static list covers the core set).
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return []string{
		"Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Turbo", "Doubao-Seed-2.0-Code",
		"seed-code-pro-0430",
		"DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "DeepSeek-V4-Flash-Official",
		"glm-5.2", "glm-5", "glm-5-turbo",
		"kimi-k3", "kimi-k2.7-code", "kimi-k2.6",
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
// skew; our refresh task runs more often, so the skew gates it.
const traeRefreshSkew = 24 * time.Hour

// Refresh rotates the Cloud-IDE JWT through ExchangeToken (refresh token
// rotates too — reference refreshLocked parity). This used to be a stub that
// never refreshed: an imported token silently expired while the ug check-in
// still "passed" through its !enable branch, and chat failed with code=1001
// on every attempt (ticket 37). Leaves creds untouched on any failure so the
// old refresh token stays retryable.
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	return d.refreshToken(ctx, acc, false)
}

// refreshToken is the shared ExchangeToken core. force=true skips the expiry
// skew (used by Stream's self-heal after an auth rejection: the stored expiry
// can look fine while the JWT has been superseded upstream).
func (d *Driver) refreshToken(ctx context.Context, acc *model.Account, force bool) error {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return err
	}
	if !force && creds.ExpiresAt != 0 && time.Until(time.Unix(creds.ExpiresAt, 0)) > traeRefreshSkew {
		return nil // still fresh — nothing to do
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
	acc.CredsJSON = rawCreds
	acc.CredsDirty = true // persisted by the refresh task / gateway after success
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
