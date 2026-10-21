// AutoClaw sandbox relay: the only working chat path on current deployments
// (the userapi proxy is WAF-406 for route models, the sandbox OpenAI endpoint
// answers 404). The relay talks to the OpenClaw agent inside the account's
// sandbox: bring the device online over WebSocket (auth.inject), POST
// /api/electron/agent/send, and translate /api/events into IR.
//
// Session continuity: the upstream agent keeps its own conversation state
// keyed by sessionKey, so the gateway's sticky SessionKey is forwarded as-is
// (the relay itself only carries the latest user message — history lives in
// the agent's session, not in this request).
package autoclaw

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// relayFallbackAllowed reports whether a proxy/sandbox failure may be retried
// through the sandbox relay. Auth, credit and rate limits are terminal — the
// relay uses the same account and would fail the same way.
func relayFallbackAllowed(err error) bool {
	ue, ok := uperr.As(err)
	if !ok {
		return true
	}
	switch ue.Class {
	case uperr.ClassAuth, uperr.ClassHardCredit, uperr.ClassSoftRate, uperr.ClassCanceled, uperr.ClassUpstreamTimeout:
		return false
	}
	return true
}

// relayChatDirective keeps the sandbox agent in plain-chat mode: without it
// the OpenClaw agent treats complex prompts as tasks (plans, reads skills,
// writes artifacts) and the relay stream may never terminate. The gateway is a
// chat API, so tool/file behaviour is explicitly suppressed.
const relayChatDirective = "直接回答用户的问题，使用纯文本回复。不要使用任何工具，不要读取或创建文件，不要输出执行计划或技能说明。\n\n"

func (d *Driver) chatRelay(ctx context.Context, call driver.ChatCall, cr *Creds, emit driver.Emit) (*ir.ChatResponse, error) {
	endpoint, err := d.ensureSandbox(ctx, call.Account, cr)
	if err != nil {
		return nil, err
	}
	if bare, ok := bareRelayBase(endpoint); ok && cr.SandboxID != "" {
		_ = d.wakeSandbox(ctx, cr, bare)
	}
	bare, _ := bareRelayBase(endpoint)
	// Best-effort device online (reference parity: the HTTP bridge below may
	// still work when the device is already online).
	if ws, werr := d.openDeviceWS(ctx, cr, bare); werr != nil {
		// Keep the relay attempt: the events endpoint may still answer.
		_ = werr
	} else if ws != nil {
		defer ws.Close()
		go drainWS(ws)
	}
	events, err := d.openRelayEvents(ctx, cr, endpoint)
	if err != nil {
		return nil, err
	}
	defer events.Body.Close()

	sessionKey := relaySessionKey(call.Request)
	args := map[string]any{"sessionKey": sessionKey, "message": relayChatDirective + lastUserText(call.Request)}
	if eff := strings.TrimSpace(call.Request.RequestedEffort); eff != "" {
		args["thinking"] = eff
	}
	sendBody, _ := json.Marshal(map[string]any{"args": []any{args}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/api/electron/agent/send", bytes.NewReader(sendBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", cr.bearer())
	modelID := stripRoutePrefix(strings.TrimSpace(call.UpstreamModel))
	if modelID != "" && !agentTarget(modelID) {
		req.Header.Set("x-openclaw-model", modelID)
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw relay: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env struct {
		OK    *bool           `json:"ok"`
		Code  int             `json:"code"`
		Error string          `json:"error"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
		RunID string          `json:"runId"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, uperr.New(uperr.ClassTransport, resp.StatusCode, "autoclaw relay: decode send: %v", err)
	}
	if (env.OK != nil && !*env.OK) || (env.Code != 0) {
		msg := firstNonEmpty(env.Error, env.Msg, string(raw))
		return nil, uperr.New(uperr.ClassTransport, resp.StatusCode, "autoclaw relay: %s", clipText(msg, 240))
	}
	runID := env.RunID
	if runID == "" && len(env.Data) > 0 {
		var s string
		if json.Unmarshal(env.Data, &s) == nil {
			runID = s
		} else {
			var obj map[string]any
			if json.Unmarshal(env.Data, &obj) == nil {
				runID = firstNonEmpty(toStringAny(obj["runId"]), toStringAny(obj["run_id"]))
			}
		}
	}
	streamBody := httpx.WatchIdle(events.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return d.convertRelay(ctx, streamBody, runID, sessionKey, call, emit)
}

func (d *Driver) openDeviceWS(ctx context.Context, cr *Creds, bare string) (*websocket.Conn, error) {
	if cr.SandboxID == "" {
		return nil, fmt.Errorf("no sandbox id")
	}
	base := strings.TrimRight(bare, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + base[len("https://"):]
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + base[len("http://"):]
	}
	token := stripBearer(cr.AccessToken)
	u := base + "/v1/client/ws?device_id=" + url.QueryEscape(cr.SandboxID) + "&access_token=" + url.QueryEscape(token)
	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true}, // upstream sandbox cert chain (reference parity)
	}
	conn, _, err := dialer.DialContext(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	frame := map[string]any{
		"type":           "auth.inject",
		"token":          token,
		"clientMetadata": map[string]any{"client_type": "web"},
	}
	if cr.UID != "" {
		frame["userId"] = cr.UID
	}
	if cr.UserName != "" {
		frame["userName"] = cr.UserName
	}
	if err := conn.WriteJSON(frame); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(25 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("auth wait: %w", err)
		}
		var m map[string]any
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch toStringAny(m["type"]) {
		case "auth.inject.ok":
			_ = conn.SetReadDeadline(time.Time{})
			return conn, nil
		case "auth.inject.error":
			conn.Close()
			return nil, fmt.Errorf("auth.inject failed")
		}
	}
}

// drainWS keeps control frames flowing while the relay request runs.
func drainWS(conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (d *Driver) openRelayEvents(ctx context.Context, cr *Creds, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", cr.bearer())
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw relay events: %v", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	return resp, nil
}

// convertRelay turns the agent event stream into IR events.
func (d *Driver) convertRelay(ctx context.Context, r io.Reader, runID, sessionKey string, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: time.Now().Unix(), Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf   strings.Builder
		thinkBuf  strings.Builder
		tools     = map[string]*ir.ToolCall{}
		toolOrder []string
		toolSeq   int
		usage     ir.Usage
		gotUsage  bool
		lastText  string
		done      bool
		streamErr error
	)
	finish := func() error {
		if done {
			return nil
		}
		done = true
		if streamErr != nil {
			return streamErr
		}
		// A silent stream (no events at all) means the sandbox/agent did not
		// produce a completion (offline, invalid x-openclaw-model, or a failed
		// run). Returning an empty success would mask the failure and block
		// channel fallback.
		if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolOrder) == 0 && !gotUsage {
			streamErr = uperr.New(uperr.ClassTransport, 0,
				"autoclaw relay: empty completion (sandbox offline or model unavailable)")
			return streamErr
		}
		for _, id := range toolOrder {
			out.Message.ToolCalls = append(out.Message.ToolCalls, *tools[id])
		}
		stop := ir.StopEnd
		if len(out.Message.ToolCalls) > 0 {
			stop = ir.StopToolUse
		}
		out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
		out.Thinking = thinkBuf.String()
		out.StopReason = stop
		out.Usage = usage
		if gotUsage {
			if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &usage}); err != nil {
				return err
			}
		}
		return emit(ir.StreamEvent{Kind: ir.EvDone, Stop: stop})
	}
	handleText := func(text string) {
		if text == "" || text == lastText {
			return
		}
		delta := text
		if strings.HasPrefix(text, lastText) {
			delta = text[len(lastText):]
		}
		lastText = text
		if delta == "" {
			return
		}
		textBuf.WriteString(delta)
		if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: delta}); err != nil && streamErr == nil {
			streamErr = err
		}
	}
	handleThinking := func(text string) {
		if text == "" {
			return
		}
		thinkBuf.WriteString(text)
		if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: text}); err != nil && streamErr == nil {
			streamErr = err
		}
	}
	handleTool := func(tid, name string, input any) {
		if name == "" {
			return
		}
		if tid == "" {
			tid = fmt.Sprintf("%s-%d", name, toolSeq)
		}
		if _, ok := tools[tid]; ok {
			return
		}
		args := ""
		if input != nil {
			raw, err := json.Marshal(input)
			if err == nil {
				args = string(raw)
			}
		}
		tc := &ir.ToolCall{ID: tid, Name: name, Arguments: args}
		tools[tid] = tc
		toolOrder = append(toolOrder, tid)
		toolSeq++
		if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: toolSeq - 1, ToolCall: &ir.ToolCall{ID: tid, Name: name}}); err != nil && streamErr == nil {
			streamErr = err
		}
		if args != "" {
			if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: toolSeq - 1, Delta: args}); err != nil && streamErr == nil {
				streamErr = err
			}
		}
	}
	handleUsage := func(m map[string]any) {
		if m == nil {
			return
		}
		pick := func(keys ...string) int {
			for _, k := range keys {
				if n, ok := anyInt(m[k]); ok {
					return n
				}
			}
			return 0
		}
		in, outN := pick("input", "prompt_tokens", "inputTokens"), pick("output", "completion_tokens", "outputTokens")
		if in == 0 && outN == 0 {
			return
		}
		usage = ir.Usage{InputTokens: in, OutputTokens: outN}
		gotUsage = true
	}

	dispatch := func(ev map[string]any) {
		payload, _ := ev["payload"].(map[string]any)
		if payload == nil {
			return
		}
		pr := toStringAny(payload["runId"])
		sk := toStringAny(payload["sessionKey"])
		to := toStringAny(payload["to"])
		sameRun := pr != "" && runID != "" && pr == runID
		sameSession := (sk != "" && sk == sessionKey) || (to != "" && to == sessionKey)
		if !sameRun && !(sameSession && pr == "") {
			return
		}
		switch toStringAny(ev["event"]) {
		case "agent:stream", "stream", "chat:message", "chat.delta":
			switch toStringAny(payload["type"]) {
			case "error":
				streamErr = uperr.New(uperr.ClassTransport, 0, "autoclaw relay: %s", payloadMessage(payload))
				_ = finish()
			case "done":
				handleUsage(extractUsageMap(payload))
				_ = finish()
			case "text":
				handleText(firstNonEmpty(toStringAny(payload["delta"]), toStringAny(payload["text"])))
			case "thinking":
				handleThinking(firstNonEmpty(toStringAny(payload["text"]), toStringAny(payload["delta"])))
			case "tool_call", "tool_use":
				handleTool(toStringAny(payload["id"]), firstNonEmpty(toStringAny(payload["name"]), toStringAny(payload["tool"])), payload["input"])
			}
			if msg, ok := payload["message"].(map[string]any); ok {
				if c := toStringAny(msg["content"]); c != "" {
					handleText(c)
				}
			}
		case "chat":
			if toStringAny(payload["state"]) == "error" {
				streamErr = uperr.New(uperr.ClassTransport, 0, "autoclaw relay: %s", payloadMessage(payload))
				_ = finish()
				return
			}
			if msg, ok := payload["message"].(map[string]any); ok {
				if c := toStringAny(msg["content"]); c != "" {
					handleText(c)
				}
				if tcs, ok := msg["toolCalls"].([]any); ok {
					for _, raw := range tcs {
						tc, _ := raw.(map[string]any)
						if tc == nil {
							continue
						}
						name := toStringAny(tc["name"])
						if name == "" {
							if fn, ok := tc["function"].(map[string]any); ok {
								name = toStringAny(fn["name"])
							}
						}
						handleTool(toStringAny(tc["id"]), name, tc["input"])
					}
				}
			}
			if toStringAny(payload["state"]) == "final" {
				handleUsage(extractUsageMap(payload))
				_ = finish()
			}
		case "agent":
			data, _ := payload["data"].(map[string]any)
			if data == nil {
				data = payload
			}
			switch toStringAny(payload["stream"]) {
			case "assistant":
				handleText(firstNonEmpty(toStringAny(data["text"]), toStringAny(data["delta"])))
				if parts, ok := data["content"].([]any); ok {
					for _, raw := range parts {
						p, _ := raw.(map[string]any)
						if p == nil {
							continue
						}
						switch toStringAny(p["type"]) {
						case "text":
							handleText(toStringAny(p["text"]))
						case "thinking":
							handleThinking(toStringAny(p["thinking"]))
						case "tool_use", "toolCall":
							handleTool(toStringAny(p["id"]), toStringAny(p["name"]), p["input"])
						}
					}
				}
			case "tool":
				if toStringAny(data["phase"]) == "start" {
					handleTool(toStringAny(data["toolCallId"]), toStringAny(data["name"]), data["args"])
				}
			case "lifecycle":
				if toStringAny(data["phase"]) == "end" {
					handleUsage(extractUsageMap(data))
					_ = finish()
				} else if toStringAny(data["phase"]) == "error" {
					streamErr = uperr.New(uperr.ClassTransport, 0, "autoclaw relay: %s", payloadMessage(data))
					_ = finish()
				}
			case "error":
				streamErr = uperr.New(uperr.ClassTransport, 0, "autoclaw relay: %s", payloadMessage(data))
				_ = finish()
			}
		}
	}

	var buf []string
	for sc.Scan() {
		if streamErr != nil {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "autoclaw relay: %v", err)
		}
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			if len(buf) > 0 {
				var data strings.Builder
				for _, l := range buf {
					if strings.HasPrefix(l, "data:") {
						data.WriteString(strings.TrimSpace(strings.TrimPrefix(l, "data:")))
					}
				}
				if data.Len() > 0 {
					var ev map[string]any
					if json.Unmarshal([]byte(data.String()), &ev) == nil {
						dispatch(ev)
					}
				}
				buf = nil
			}
			if done {
				break
			}
			continue
		}
		buf = append(buf, line)
	}
	if err := sc.Err(); err != nil && streamErr == nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0, "autoclaw relay: stream stalled")
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "autoclaw relay: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw relay: %v", err)
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "autoclaw relay: canceled: %v", ctx.Err())
	}
	if !done {
		return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw relay: stream ended without terminal signal")
	}
	return out, nil
}

// relaySessionKey returns the sandbox agent session key for a request. The
// upstream agent keeps its own conversation state per key, so unrelated chats
// MUST NOT share one: the old "main" fallback pooled every session into a
// single agent context and answers/images crossed over between conversations
// (2026-10-20). With no stable identity we mint a fresh key per request —
// continuity is lost, contamination is not.
func relaySessionKey(req *ir.ChatRequest) string {
	if req != nil {
		if k := strings.TrimSpace(req.SessionKey); k != "" {
			return k
		}
	}
	return relayRandomKey()
}

// relayRandomKey mints a per-request sandbox session key for requests that
// carry no stable conversation identity.
func relayRandomKey() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func lastUserText(req *ir.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != ir.RoleUser {
			continue
		}
		var b strings.Builder
		for _, p := range m.Content {
			if p.Kind == "text" {
				b.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func extractUsageMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if u, ok := m["usage"].(map[string]any); ok {
		return u
	}
	if _, ok := m["input"]; ok {
		return m
	}
	if _, ok := m["prompt_tokens"]; ok {
		return m
	}
	if r, ok := m["result"].(map[string]any); ok {
		if u, ok := r["usage"].(map[string]any); ok {
			return u
		}
		return r
	}
	return nil
}

func payloadMessage(m map[string]any) string {
	if m == nil {
		return "upstream error event (no detail)"
	}
	for _, k := range []string{"message", "error", "msg", "detail", "text", "delta"} {
		if s := toStringAny(m[k]); s != "" {
			return clipText(s, 240)
		}
	}
	return "upstream error event"
}

func anyInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		n, err := x.Int64()
		return int(n), err == nil
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func toStringAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
