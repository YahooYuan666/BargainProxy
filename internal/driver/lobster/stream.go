// Streaming chat path for the lobster channel.
//
// The upstream is OpenAI-shaped on the wire but has three behaviours the
// generic openai-compat parser cannot survive:
//
//   - it always streams (a stream:false request still gets SSE),
//   - it reports failures as `event: error` inside HTTP 200,
//   - its thinking gears live in a private `lobsterai_options` field.
//
// This file therefore owns its own SSE loop instead of reusing
// openaicompat.parseSSE.

package lobster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// thinkingGears maps the gateway's portable effort vocabulary onto the
// upstream's private gear names. The upstream only accepts off|high|max.
var thinkingGears = map[string]string{
	"none":    "off",
	"off":     "off",
	"minimal": "high",
	"low":     "high",
	"medium":  "high",
	"high":    "high",
	"xhigh":   "max",
	"max":     "max",
}

// Stream implements driver.Driver.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	c, err := d.credsOf(call.Account)
	if err != nil {
		return nil, err
	}
	body, err := d.buildBody(ctx, call, c)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "lobster: build body: %v", err)
	}
	// Upstream hard ceilings (BP-103, probed 2026-10-24): the proxy kills any
	// request whose JSON body exceeds ~5 MB, and any prompt over 1,048,576
	// tokens (2^20; the catalog's contextWindow says 1000000). Both come back
	// the same lying way — 17-67s of silence, then HTTP 500 with an EMPTY
	// body — which the gateway can only read as a transport failure and the
	// client reads as "channel dead". Probes that separate the two axes:
	// 20 MB of "aaaa" (few tokens) still dies, so the byte limit is real; a
	// CJK ladder read back prompt_tokens=1048263 → 200 while the next step
	// died, so the token limit is 2^20. Gate here, before the socket opens,
	// and reject as bad_request: not rotatable within the pool (every account
	// hits the same ceiling) but cascading to another channel continues.
	if err := rejectOversize(body, ir.EstimatePromptTokens(call.Request)); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Ask for a stream explicitly: the upstream streams regardless, and
	// accepting it keeps the transport honest about what is coming back.
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	d.clientHeaders(req)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "lobster: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// BP-241: scope a soft rate limit to the MODEL. An account-wide rest
		// for one model's wall freezes every other model the account serves —
		// the same defect BP-189 fixed on workbuddy and BP-240 on trae.
		e := classifyHTTP(resp.StatusCode, raw)
		if e != nil && e.Class == uperr.ClassSoftRate && e.Model == "" {
			e.Model = call.UpstreamModel
		}
		return nil, e
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return parseStream(ctx, streamBody, call, emit)
}

// buildBody assembles the OpenAI-shaped request with the channel's private
// extensions. Everything the upstream rejects is removed here rather than
// discovered as a 400.
func (d *Driver) buildBody(ctx context.Context, call driver.ChatCall, c *Creds) ([]byte, error) {
	req := call.Request
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		wire := map[string]any{"role": string(m.Role)}
		switch {
		case m.Role == ir.RoleTool:
			// Tool results are plain strings; image parts in a tool message
			// would be rejected (BP-70 lesson from workbuddy).
			wire["content"] = ir.TextOf(m.Content)
		case len(m.Content) == 1 && m.Content[0].Kind == "text":
			wire["content"] = m.Content[0].Text
		case len(m.Content) > 0:
			parts := make([]map[string]any, 0, len(m.Content))
			for _, p := range m.Content {
				switch p.Kind {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				case "image":
					parts = append(parts, map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": "data:" + p.ImageMIME + ";base64," + p.ImageB64,
						},
					})
				}
			}
			wire["content"] = parts
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
				})
			}
			wire["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		// Kimi K3 refuses an assistant tool-call turn whose history lacks a
		// reasoning_content field (kimiK3StreamWrapper.ts). An empty string is
		// accepted; omitting the key is not.
		if isKimiK3(call.UpstreamModel) && m.Role == ir.RoleAssistant && len(m.ToolCalls) > 0 {
			if _, ok := wire["reasoning_content"]; !ok {
				wire["reasoning_content"] = ""
			}
		}
		messages = append(messages, wire)
	}
	body := map[string]any{
		"model":    call.UpstreamModel,
		"messages": messages,
		"stream":   true, // upstream streams regardless; be explicit
	}
	if req.Temperature != nil && !isKimiK3(call.UpstreamModel) {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil && !isKimiK3(call.UpstreamModel) {
		body["top_p"] = *req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = req.ToolChoice
	}

	// Thinking controls.
	if isKimiK3(call.UpstreamModel) {
		// K3 fixes its own gear: sampling is server-decided and reasoning_effort
		// must be exactly "max". Sending lobsterai_options is rejected because
		// K3's catalog entry carries no thinkingConfig.
		body["reasoning_effort"] = "max"
	} else if gear, ok := thinkingGear(req); ok {
		// The field is legal ONLY for models whose catalog entry declares a
		// thinkingConfig; for the other 22 catalog models it is a hard 400
		// (code 4000). Ask the catalog rather than trusting the caller — the
		// gateway sets an effort for many models that have no gear table.
		if capable := d.thinkingModels(ctx); capable != nil {
			if capable[call.UpstreamModel] {
				body[optionsField] = map[string]any{
					"version":  optionsVersion,
					"thinking": map[string]any{"level": gear},
				}
			}
			// Not in the set: send nothing. The upstream applies its own
			// default thinking instead of rejecting the turn.
		}
	}
	return json.Marshal(body)
}

const (
	optionsField   = "lobsterai_options"
	optionsVersion = 1
)

// Hard upstream ceilings probed on 2026-10-24 (BP-103). The proxy returns an
// empty 500 after 17-67s for requests over either limit, so both are gated
// before the socket opens. The byte margin is generous (4 MB gate vs a
// measured death at 5 MB and survival at 4.5 MB) because the true edge sits
// somewhere in between and the cost of a false rejection is one skipped
// channel while the cost of a miss is a minute of dead air. The token gate
// mirrors the measured 2^20 with the same headroom.
const (
	maxBodyBytes = 4 << 20   // measured: 4.5 MB survives, 5 MB dies
	maxPromptEst = 1_000_000 // measured ceiling: 1,048,576 (2^20)
)

// rejectOversize reports whether the request would be killed by the proxy's
// body-size ceiling or the model's context ceiling. estTokens is the shared
// ir.EstimatePromptTokens heuristic (chars/2 — an upper bound for CJK, lower
// for repetitive ASCII), deliberately conservative: this gate decides "try
// another channel", not "fail the turn".
func rejectOversize(body []byte, estTokens int) error {
	if len(body) > maxBodyBytes {
		return uperr.New(uperr.ClassBadRequest, 413,
			"lobster: request body of %d bytes exceeds the proxy ceiling of %d bytes; upstream answers an empty 500 after a long hang, try another channel",
			len(body), maxBodyBytes)
	}
	if estTokens > maxPromptEst {
		return uperr.New(uperr.ClassBadRequest, 413,
			"lobster: estimated prompt of %d tokens exceeds the context ceiling of %d; upstream answers an empty 500 after a long hang, try another channel",
			estTokens, maxPromptEst)
	}
	return nil
}

// thinkingGear decides whether to send the private thinking field and which
// gear to use. It returns ok=false when the caller asked for nothing, so the
// upstream default applies.
//
// The gear table is NOT consulted here: the driver has no catalog access, and
// the reference plugin's rule is that the field is only legal for models whose
// entry declared a thinkingConfig. Callers express that by binding such models
// only; when in doubt the caller's explicit effort is honoured because the
// gateway only sets it for models the operator chose.
func thinkingGear(req *ir.ChatRequest) (string, bool) {
	level := ""
	if req.Thinking != nil {
		if req.Thinking.Enabled && req.Thinking.Level != "" {
			level = strings.ToLower(strings.TrimSpace(req.Thinking.Level))
		} else if !req.Thinking.Enabled && req.Thinking.Level != "" {
			// An explicit "thinking off" is a real instruction: the upstream
			// treats level=off as genuinely disabling reasoning.
			level = "off"
		}
	}
	if level == "" && req.RequestedEffort != "" {
		level = strings.ToLower(strings.TrimSpace(req.RequestedEffort))
	}
	if level == "" {
		return "", false
	}
	if g, ok := thinkingGears[level]; ok {
		return g, true
	}
	return "", false
}

// isKimiK3 reports whether the model is the Kimi K3 family, which has its own
// transport contract (fixed sampling, forced max reasoning).
func isKimiK3(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return m == "kimi-k3" || strings.HasPrefix(m, "kimi-k3-") || strings.HasPrefix(m, "kimi-k3.")
}

// parseStream consumes the upstream SSE stream.
func parseStream(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)

	out := &ir.ChatResponse{
		Model:     call.Request.Model,
		CreatedAt: time.Now().Unix(),
		Message:   ir.Message{Role: ir.RoleAssistant},
	}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		tools      = map[int]*ir.ToolCall{}
		toolOrder  []int
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool // a finish_reason was seen
		done       bool // the stream declared [DONE] (safe to stop reading)
		event      string
		dataLines  []string
	)

	// dispatch handles one complete SSE event. Returning an error aborts.
	dispatch := func() error {
		if len(dataLines) == 0 {
			event = ""
			return nil
		}
		data := strings.Join(dataLines, "\n")
		ev := event
		event, dataLines = "", nil

		// The upstream hides failures inside a 200 stream. Every event is
		// checked, not just the first: a mid-stream error must fail the turn
		// so the pool rotates instead of recording a success.
		if ev == "error" {
			if fe := frameErrorOf(data); fe != nil {
				return fe
			}
			return uperr.New(uperr.ClassTransport, 0, "lobster: upstream error event: %s", truncate([]byte(data), 200))
		}
		if data == "[DONE]" {
			terminated = true
			done = true
			return nil
		}
		// Some deployments omit `event: error` and just put an error object in
		// data; catch that shape too.
		if fe := frameErrorOf(data); fe != nil && !strings.Contains(data, `"choices"`) {
			return fe
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
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil // a keep-alive or a shape we do not model
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
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := tools[tc.Index]
				if !ok {
					acc = &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					tools[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
					if err := emit(ir.StreamEvent{
						Kind: ir.EvToolStart, ToolIndex: tc.Index,
						ToolCall: &ir.ToolCall{ID: acc.ID, Name: acc.Name},
					}); err != nil {
						return err
					}
				}
				if acc.ID == "" && tc.ID != "" {
					acc.ID = tc.ID
				}
				if acc.Name == "" && tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.Arguments += tc.Function.Arguments
					if err := emit(ir.StreamEvent{
						Kind: ir.EvToolArgs, ToolIndex: tc.Index, Delta: tc.Function.Arguments,
					}); err != nil {
						return err
					}
				}
			}
			if ch.FinishReason != "" {
				// BP-222: finish_reason "error" is the vendor's word for a
				// failed turn — fail the attempt, never report it as a success.
				if ir.FinishReasonFailed(ch.FinishReason) {
					return uperr.NewFailedFinishTurn("lobster")
				}
				terminated = true
				stop = mapFinish(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			usage = ir.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				usage.CachedInputTokens = d.CachedTokens
				usage.CacheKnown = true
			}
			if d := chunk.Usage.CompletionTokensDetails; d != nil {
				usage.ReasoningTokens = d.ReasoningTokens
			}
			gotUsage = true
		}
		return nil
	}

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "lobster: stream canceled: %v", err)
		}
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return nil, err
			}
			// Do NOT stop on finish_reason alone. qwen/kimi send the terminal
			// usage in a SEPARATE trailing chunk (choices:[]) AFTER the
			// finish_reason chunk; breaking here dropped it and every such
			// turn reported zero tokens (2026-10-23, field).
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"lobster: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "lobster: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster: stream read: %v", err)
	}
	// Flush a trailing event that arrived without its blank line.
	if !terminated {
		if err := dispatch(); err != nil {
			return nil, err
		}
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "lobster: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster: stream ended without terminal signal")
	}

	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *tools[idx])
	}
	if len(out.Message.ToolCalls) > 0 && (stop == ir.StopEnd || stop == "") {
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
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

func readLimited(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, 1<<20))
}

var _ = fmt.Sprintf
