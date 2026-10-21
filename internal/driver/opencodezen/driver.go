// Driver: OpenCode Zen chat-completions lane. Chat requests are reshaped for
// the free lane (forced streaming + gate tools + effort filtering), posted
// with the disguise headers, and streamed back as IR events. Tool calls to
// injected gate tools are stripped before they reach the client.
package opencodezen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// zenUpstreamBase is the anonymous free-lane base. Overridable for tests.
const zenUpstreamBase = "https://opencode.ai/zen"

// Driver implements driver.Driver for the opencodezen channel.
type Driver struct {
	cfg  config.OpenCodeZenSettings
	http *http.Client
	rpc  *http.Client
	// gateEnabled can be turned off for non-free (probed) models if upstream
	// ever opens a paid lane; today every request goes through the gate.
	gateEnabled atomic.Bool
	// wireFor resolves a model id to its upstream lane ("chat" / "responses";
	// "" = not servable). Wired from the catalog in main; nil means chat.
	wireFor func(string) string
}

// SetWireResolver wires the catalog's lane decision into the driver.
func (d *Driver) SetWireResolver(f func(string) string) { d.wireFor = f }

// New builds the driver.
func New(cfg config.OpenCodeZenSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	d := &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient()}
	d.gateEnabled.Store(true)
	return d
}

// Channel reports the channel id.
func (d *Driver) Channel() string { return model.ChannelOpenCodeZen }

// Probe is a lightweight liveness check against the model list endpoint.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.base(), "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	for _, h := range disguiseHeaders(deriveRequestIDs([]byte(`{"messages":[]}`))) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "opencodezen: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, []byte{})
	}
	return &driver.ProbeResult{}, nil
}

func (d *Driver) base() string {
	if d.cfg.BaseURL != "" {
		return d.cfg.BaseURL
	}
	return zenUpstreamBase
}

// Models lists the upstream model catalog (best-effort; used by the
// directory sync task — BP-54).
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.base(), "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	for _, h := range disguiseHeaders(deriveRequestIDs([]byte(`{"messages":[]}`))) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "opencodezen: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "opencodezen: read models: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, uperr.New(uperr.ClassInternal, 200, "opencodezen: models body: %v", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Upstream lanes. Most free-lane models answer on /v1/chat/completions; a few
// (muse-spark*, whose models.dev SDK is @ai-sdk/openai) only answer on
// /v1/responses — the chat lane answers 400 ModelProtocolUnsupported for them.
const (
	laneChat        = "chat"
	laneResponses   = "responses"
	laneUnsupported = "" // the model has no supported upstream lane (e.g. jev)
)

func (d *Driver) lane(model string) string {
	if d.wireFor == nil {
		return laneChat
	}
	if l := d.wireFor(model); l != "" {
		return l
	}
	return laneUnsupported
}

// Stream performs one chat request on the model's own upstream lane: reshape →
// POST → parse SSE → IR.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	switch d.lane(call.UpstreamModel) {
	case laneResponses:
		wire, err := d.buildResponsesWire(call)
		if err != nil {
			return nil, uperr.New(uperr.ClassBadRequest, 400, "opencodezen: build responses body: %v", err)
		}
		return d.responsesOnce(ctx, wire, emit)
	case laneUnsupported:
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"opencodezen: model %q is not served on a supported upstream lane", call.UpstreamModel)
	}
	wire, err := d.buildWire(call)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "opencodezen: build body: %v", err)
	}
	resp, err := d.chatOnce(ctx, wire, emit)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Upstream tool-count cap. Zen rejects any request with more than 128 tools
// (400 invalid_value "maximum number of items is 128"). A ZCode session with a
// few MCP servers easily exceeds that, and a hard failure kills the whole turn —
// keeping the first N client tools plus our two gate stubs lets the request
// through (with fewer optional tools) instead.
const (
	maxUpstreamTools = 128
	maxClientTools   = maxUpstreamTools - 2 // room for the bash/read gate stubs
)

// capTools truncates an over-long tool list, reporting whether it cut anything.
func capTools[T any](tools []T) ([]T, bool) {
	if len(tools) <= maxClientTools {
		return tools, false
	}
	return tools[:maxClientTools], true
}

// --- content & tool_choice conversion ---------------------------------------

// imageDataURL renders one image part as a base64 data URL.
func imageDataURL(p ir.ContentPart) string {
	mime := p.ImageMIME
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + p.ImageB64
}

func hasImage(parts []ir.ContentPart) bool {
	for _, p := range parts {
		if p.Kind == "image" && p.ImageB64 != "" {
			return true
		}
	}
	return false
}

// chatContentParts renders IR content as the chat lane's content array
// (text + image_url). Silently dropping images used to lose attachments.
func chatContentParts(parts []ir.ContentPart) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case "text":
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case "image":
			if p.ImageB64 != "" {
				out = append(out, map[string]any{"type": "image_url",
					"image_url": map[string]any{"url": imageDataURL(p)}})
			}
		}
	}
	return out
}

// responsesContentParts renders IR content as Responses input content items.
func responsesContentParts(parts []ir.ContentPart) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case "text":
			if p.Text != "" {
				out = append(out, map[string]any{"type": "input_text", "text": p.Text})
			}
		case "image":
			if p.ImageB64 != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": imageDataURL(p)})
			}
		}
	}
	return out
}

// toolChoiceName parses the ingress dialects into a normalized triple:
// kind is "auto" | "required" | "none" | "name", name only for "name".
// OpenAI chat sends "auto/none/required" or {type:function,function:{name}},
// Responses sends the same strings or {type:function,name}, and Anthropic says
// "any"/"tool" — forwarding those raw is an invalid upstream request.
func toolChoiceName(v any) (kind, name string) {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "required", "none":
			return t, ""
		case "any", "tool":
			return "required", ""
		}
		return "", ""
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if n, _ := fn["name"].(string); n != "" {
				return "name", n
			}
		}
		if n, _ := t["name"].(string); n != "" {
			return "name", n
		}
		switch t["type"] {
		case "any", "tool", "required":
			return "required", ""
		case "auto":
			return "auto", ""
		case "none":
			return "none", ""
		}
	}
	return "", ""
}

// chatToolChoice maps any ingress dialect onto the chat lane's accepted values.
// The specific tool name rides in IR separately after BP-69 normalization.
func chatToolChoice(v any, name string) any {
	kind, n := toolChoiceName(v)
	if name != "" {
		kind, n = "name", name
	}
	switch kind {
	case "auto", "none", "required":
		return kind
	case "name":
		return map[string]any{"type": "function", "function": map[string]any{"name": n}}
	}
	return nil
}

// responsesToolChoice maps onto the Responses lane: never "none" (upstream 400),
// and a named tool uses the flat function object.
func responsesToolChoice(v any, name string) any {
	kind, n := toolChoiceName(v)
	if name != "" {
		kind, n = "name", name
	}
	switch kind {
	case "auto", "required":
		return kind
	case "name":
		return map[string]any{"type": "function", "name": n}
	}
	return nil // "none" and unknown are omitted on purpose
}

// buildWire converts the IR request into the free-lane chat wire body:
// forced streaming, accepted efforts only, gate tools injected, injected-tool
// names remembered for response filtering.
func (d *Driver) buildWire(call driver.ChatCall) ([]byte, error) {
	req := call.Request
	var messages []map[string]any
	for _, m := range req.Messages {
		wire := map[string]any{"role": string(m.Role)}
		// Content: omitted for a tool-only assistant turn, plain string
		// otherwise. Tool fields below are emitted independently of the content
		// shape — folding them into one branch dropped tool_calls on tool-only
		// assistant turns and tool_call_id on single-text tool results, which
		// upstream rejects as an invalid request (the 400 users hit on any tool
		// round-trip).
		switch {
		case len(m.Content) == 0 && m.Role == ir.RoleAssistant && len(m.ToolCalls) > 0:
			// content stays absent
		case m.Role == ir.RoleTool:
			// OpenAI tool messages carry a plain string; image parts are not
			// representable on the chat lane (BP-70).
			wire["content"] = joinText(m.Content)
		case hasImage(m.Content):
			wire["content"] = chatContentParts(m.Content)
		case len(m.Content) == 1 && m.Content[0].Kind == "text":
			wire["content"] = m.Content[0].Text
		default:
			wire["content"] = joinText(m.Content)
		}
		if tc := m.ToolCalls; len(tc) > 0 {
			var tcs []map[string]any
			for _, c := range tc {
				tcs = append(tcs, map[string]any{
					"id":   c.ID,
					"type": "function",
					"function": map[string]any{
						"name":      c.Name,
						"arguments": c.Arguments,
					},
				})
			}
			wire["tool_calls"] = tcs
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
		"model":    call.UpstreamModel,
		"messages": messages,
		"stream":   true, // free lane hard requirement
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	// Keep this a []any: ensureGateTools reads body["tools"] as []any, and a
	// concrete []map[string]any here used to fail that assertion silently —
	// the gate tools then replaced every real tool declaration.
	var tools []any
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if t.Parameters != nil {
			fn["parameters"] = t.Parameters
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		if capped, cut := capTools(tools); cut {
			log.Printf("opencodezen: tools %d > upstream cap %d, keeping the first %d",
				len(tools), maxUpstreamTools, maxClientTools)
			tools = capped
		}
		body["tools"] = tools
		if tc := chatToolChoice(req.ToolChoice, req.ToolChoiceName); tc != nil {
			body["tool_choice"] = tc
		}
	}
	// Reasoning effort: pass through only values the upstream accepts; map the
	// IR level onto them otherwise (chat lane speaks reasoning_effort).
	if req.RequestedEffort != "" {
		if acceptedEfforts[req.RequestedEffort] {
			body["reasoning_effort"] = req.RequestedEffort
		}
	} else if req.Thinking != nil && req.Thinking.Level != "" && acceptedEfforts[req.Thinking.Level] {
		body["reasoning_effort"] = req.Thinking.Level
	}
	if d.gateEnabled.Load() {
		ensureGateTools(body, false)
	}
	return json.Marshal(body)
}

// chatOnce posts one request and parses the SSE stream.
func (d *Driver) chatOnce(ctx context.Context, wire []byte, emit driver.Emit) (*ir.ChatResponse, error) {
	ids := deriveRequestIDs(wire)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.base(), "/")+"/v1/chat/completions", bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range disguiseHeaders(ids) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "opencodezen: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	stream := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer stream.Close()
	return parseSSE(ctx, stream, emit)
}

// parseSSE converts the OpenAI-style chunk stream into IR events, dropping
// tool calls made to injected gate tools.
func parseSSE(ctx context.Context, r io.Reader, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
		toolAcc    = map[int]*ir.ToolCall{}
		toolOrder  []int
		gateOnly   = true // no real (non-gate) tool seen yet
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "opencodezen: stream canceled: %v", err)
		}
		line := strings.TrimRight(sc.Text(), "\r")
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			terminated = true
			break
		}
		if payload == "" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				// Free lane echoes prefix-cache accounting on some backends.
				PromptCacheHitTokens  *int `json:"prompt_cache_hit_tokens"`
				PromptCacheMissTokens *int `json:"prompt_cache_miss_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return nil, uperr.New(uperr.ClassInternal, 200, "opencodezen: %s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				thinkBuf.WriteString(ch.Delta.ReasoningContent)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ch.Delta.ReasoningContent}); err != nil {
					return nil, err
				}
			}
			if ch.Delta.Content != "" {
				textBuf.WriteString(ch.Delta.Content)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ch.Delta.Content}); err != nil {
					return nil, err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				if isGateToolName(tc.Function.Name) {
					continue // our injected stub: never surface it
				}
				acc, ok := toolAcc[tc.Index]
				if !ok {
					acc = &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					toolAcc[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
					if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: tc.Index,
						ToolCall: &ir.ToolCall{ID: acc.ID, Name: acc.Name}}); err != nil {
						return nil, err
					}
				}
				if tc.Function.Arguments != "" {
					acc.Arguments += tc.Function.Arguments
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: tc.Index, Delta: tc.Function.Arguments}); err != nil {
						return nil, err
					}
				}
			}
			if ch.FinishReason != "" {
				terminated = true
				stop = mapFinish(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			usage = ir.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
			if chunk.Usage.PromptCacheHitTokens != nil {
				usage.CachedInputTokens = *chunk.Usage.PromptCacheHitTokens
				usage.CacheKnown = true
			}
			gotUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"opencodezen: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "opencodezen: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "opencodezen: stream read: %v", err)
	}
	_ = gateOnly
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "opencodezen: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "opencodezen: stream ended without terminal signal")
	}

	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *toolAcc[idx])
	}
	if len(out.Message.ToolCalls) > 0 && stop == ir.StopEnd {
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolOrder) == 0 {
		log.Printf("opencodezen: empty completion (model unknown at parse site; check upstream)")
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.StopReason = stop
	if gotUsage {
		out.Usage = usage
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &usage}); err != nil {
			return nil, err
		}
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: stop}); err != nil {
		return nil, err
	}
	return out, nil
}

func mapFinish(reason string) ir.StopReason {
	switch strings.ToLower(reason) {
	case "stop":
		return ir.StopEnd
	case "length":
		return ir.StopLength
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default:
		return ir.StopEnd
	}
}

// classifyHTTP maps the free lane's status codes onto the taxonomy. The two
// free-lane-specific classes matter operationally: 403 FreeTierError means
// the disguise/gate contract changed (a one-place fix), 429
// "Endpoint is unavailable" is transient upstream capacity.
func classifyHTTP(status int, body []byte) *uperr.Error {
	text := string(body)
	switch {
	case status == 401:
		return uperr.New(uperr.ClassAuth, status, "opencodezen: %s", truncate(body, 300))
	case status == 403:
		if strings.Contains(text, "free tier") || strings.Contains(text, "FreeTier") {
			return uperr.New(uperr.ClassBadRequest, status,
				"opencodezen: free-tier gate rejected the request (disguise/gate contract may have changed): %s", truncate(body, 300))
		}
		return uperr.New(uperr.ClassWAF, status, "opencodezen: %s", truncate(body, 300))
	case status == 429:
		return uperr.New(uperr.ClassUpstreamBusy, status, "opencodezen: %s", truncate(body, 300))
	case status == 404:
		return uperr.New(uperr.ClassNotFound, status, "opencodezen: model endpoint gone: %s", truncate(body, 300))
	case status == 413:
		return uperr.New(uperr.ClassBadRequest, status, "opencodezen: %s", truncate(body, 300))
	case status == 400:
		return uperr.New(uperr.ClassBadRequest, status, "opencodezen: %s", truncate(body, 300))
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "opencodezen: %s", truncate(body, 300))
	default:
		return uperr.New(uperr.ClassInternal, status, "opencodezen: %s", truncate(body, 300))
	}
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// --- Responses lane (/v1/responses) -----------------------------------------
//
// A few free-lane models are only reachable on this lane (their models.dev SDK
// is @ai-sdk/openai; the chat lane answers 400 ModelProtocolUnsupported).
// Two traps the reference implementation already paid for: the gate tools must
// be the FLAT shape here (a nested "function" object is a 400), and
// tool_choice:"none" must never be sent (400 "only … are supported") — an
// existing "none" gets stripped by ensureGateTools.

// joinText concatenates the text parts of one message.
func joinText(parts []ir.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind != "text" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// buildResponsesWire converts the IR request into a Responses-lane body:
// system prompts ride in `instructions`, messages become input items, tool
// calls/results become function_call / function_call_output items, tools go
// out flat, streaming is forced and the gate tools are injected.
func (d *Driver) buildResponsesWire(call driver.ChatCall) ([]byte, error) {
	req := call.Request
	input := make([]any, 0, len(req.Messages))
	var instructions strings.Builder
	for _, m := range req.Messages {
		switch m.Role {
		case ir.RoleSystem:
			if text := joinText(m.Content); text != "" {
				if instructions.Len() > 0 {
					instructions.WriteString("\n\n")
				}
				instructions.WriteString(text)
			}
		case ir.RoleUser:
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": responsesContentParts(m.Content),
			})
		case ir.RoleAssistant:
			if text := joinText(m.Content); text != "" {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": text}},
				})
			}
			for _, c := range m.ToolCalls {
				input = append(input, map[string]any{
					"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": c.Arguments,
				})
			}
		case ir.RoleTool:
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": m.ToolCallID, "output": joinText(m.Content),
			})
		}
	}
	body := map[string]any{
		"model":  call.UpstreamModel,
		"input":  input,
		"stream": true, // free lane hard requirement
	}
	if instructions.Len() > 0 {
		body["instructions"] = instructions.String()
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	var tools []any
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		tool := map[string]any{"type": "function", "name": t.Name}
		if t.Description != "" {
			tool["description"] = t.Description
		}
		if t.Parameters != nil {
			tool["parameters"] = t.Parameters
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		if capped, cut := capTools(tools); cut {
			log.Printf("opencodezen: tools %d > upstream cap %d, keeping the first %d",
				len(tools), maxUpstreamTools, maxClientTools)
			tools = capped
		}
		body["tools"] = tools
		if tc := responsesToolChoice(req.ToolChoice, req.ToolChoiceName); tc != nil {
			body["tool_choice"] = tc
		}
	}
	if req.RequestedEffort != "" {
		if acceptedEfforts[req.RequestedEffort] {
			body["reasoning_effort"] = req.RequestedEffort
		}
	} else if req.Thinking != nil && req.Thinking.Level != "" && acceptedEfforts[req.Thinking.Level] {
		body["reasoning_effort"] = req.Thinking.Level
	}
	if d.gateEnabled.Load() {
		ensureGateTools(body, true)
	}
	return json.Marshal(body)
}

// responsesOnce posts one Responses-lane request and parses the SSE stream.
func (d *Driver) responsesOnce(ctx context.Context, wire []byte, emit driver.Emit) (*ir.ChatResponse, error) {
	ids := deriveRequestIDs(wire)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.base(), "/")+"/v1/responses", bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range disguiseHeaders(ids) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "opencodezen: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	stream := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer stream.Close()
	return parseResponsesSSE(ctx, stream, emit)
}

// responsesEvent is the subset of the Responses stream events we consume.
type responsesEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	Item        *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// parseResponsesSSE converts the Responses stream into IR events. Text and
// thinking ride their delta events; tool calls accumulate from
// output_item/arguments deltas, with the terminal `response.completed` object
// as the authoritative fallback (used only when no delta carried the call, so
// nothing is duplicated).
func parseResponsesSSE(ctx context.Context, r io.Reader, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		usage      ir.Usage
		gotUsage   bool
		terminated bool
		stop       ir.StopReason
		toolAcc    = map[int]*ir.ToolCall{}
		toolOrder  []int
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "opencodezen: stream canceled: %v", err)
		}
		line := strings.TrimRight(sc.Text(), "\r")
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev.Error != nil && ev.Error.Message != "" {
			return nil, uperr.New(uperr.ClassInternal, 200, "opencodezen: %s", ev.Error.Message)
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				textBuf.WriteString(ev.Delta)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ev.Delta}); err != nil {
					return nil, err
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if ev.Delta != "" {
				thinkBuf.WriteString(ev.Delta)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ev.Delta}); err != nil {
					return nil, err
				}
			}
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" && !isGateToolName(ev.Item.Name) {
				id := ev.Item.CallID
				if id == "" {
					id = ev.Item.ID
				}
				toolAcc[ev.OutputIndex] = &ir.ToolCall{ID: id, Name: ev.Item.Name}
				toolOrder = append(toolOrder, ev.OutputIndex)
				if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: ev.OutputIndex,
					ToolCall: &ir.ToolCall{ID: id, Name: ev.Item.Name}}); err != nil {
					return nil, err
				}
			}
		case "response.function_call_arguments.delta":
			if acc, ok := toolAcc[ev.OutputIndex]; ok && ev.Delta != "" {
				acc.Arguments += ev.Delta
				if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: ev.OutputIndex, Delta: ev.Delta}); err != nil {
					return nil, err
				}
			}
		case "response.completed", "response.incomplete":
			terminated = true
			if ev.Response != nil {
				if ev.Response.Status == "incomplete" {
					// Reasoning-heavy models can spend the whole output budget
					// thinking and never emit text, then end with incomplete
					// instead of completed — treat it as a length stop.
					stop = ir.StopLength
				}
				if u := ev.Response.Usage; u != nil {
					usage = ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
					if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
						usage.CachedInputTokens = *u.InputTokensDetails.CachedTokens
						usage.CacheKnown = true
					}
					gotUsage = true
				}
				if len(toolOrder) == 0 {
					for _, item := range ev.Response.Output {
						if item.Type != "function_call" || isGateToolName(item.Name) {
							continue
						}
						id := item.CallID
						if id == "" {
							id = item.Name
						}
						idx := len(toolOrder)
						toolAcc[idx] = &ir.ToolCall{ID: id, Name: item.Name, Arguments: item.Arguments}
						toolOrder = append(toolOrder, idx)
						if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: idx,
							ToolCall: &ir.ToolCall{ID: id, Name: item.Name}}); err != nil {
							return nil, err
						}
						if item.Arguments != "" {
							if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: idx, Delta: item.Arguments}); err != nil {
								return nil, err
							}
						}
					}
				}
			}
		case "response.failed":
			return nil, uperr.New(uperr.ClassInternal, 200, "opencodezen: responses lane reported failure")
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"opencodezen: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "opencodezen: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "opencodezen: stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "opencodezen: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "opencodezen: responses stream ended without terminal signal")
	}

	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *toolAcc[idx])
	}
	if len(out.Message.ToolCalls) > 0 && (stop == "" || stop == ir.StopEnd) {
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolOrder) == 0 {
		log.Printf("opencodezen: empty completion (responses lane; check upstream)")
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.StopReason = stop
	if gotUsage {
		out.Usage = usage
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &usage}); err != nil {
			return nil, err
		}
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: stop}); err != nil {
		return nil, err
	}
	return out, nil
}
