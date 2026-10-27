package workbuddy

// BP-131: the CN deployment serves the Claude models on an Anthropic-native
// route, <chatBase>/v1/messages, and ONLY there do their thinking gears work:
// the chat endpoint answers the same at every reasoning_effort level, while
// /v1/messages takes output_config.effort up to "max" (magpie workbuddy
// 0.1.32, measured 2026-10-24 on the China build: claude-opus-5.5,
// claude-sonnet-5.5, claude-opus-5 and claude-opus-4.8-1m answered in
// Anthropic's stream; glm-5.3 and deepseek-v4.1-flash answered 500 there and
// gpt-5.5/kimi-k3-1 400 — so the lane is gated to claude-* only). The
// international site has no such route at all ("404 Route Not Found"),
// whatever the model.
//
// Everything here degrades to the chat lane: a missing route, a channel-check
// refusal (11128) or a build failure falls back once and pins the decision
// for the process, so a deployment without the route behaves exactly as
// before plus one probe request.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// messagesPath is the Anthropic-native route.
const messagesPath = "/v1/messages"

// claudeOnMessages reports whether this (account, model) rides the
// Anthropic-native route: CN deployment, claude-* model.
func claudeOnMessages(creds *Creds, upstreamModel string) bool {
	return creds.RealmOrDefault() != "global" &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(upstreamModel)), "claude-")
}

// messagesEffort maps the client's gear onto output_config.effort. max is
// accepted on this lane (measured: claude-opus-5.5 at effort max produced
// 11248 thinking tokens against 1999 at xhigh); off/none maps to nothing —
// an absent output_config is the upstream default.
func messagesEffort(effort string) (string, bool) {
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return effort, true
	}
	return "", false
}

// messagesRouteDead reports an upstream answer that means "this lane does not
// exist here" — as opposed to a genuine failure the pool must see (rate
// limit, credit, auth, a real bad request).
func messagesRouteDead(ue *uperr.Error) bool {
	if ue.Status == http.StatusNotFound {
		return true
	}
	if ue.Status == http.StatusBadRequest {
		lower := strings.ToLower(ue.Message)
		return strings.Contains(lower, "route") || strings.Contains(lower, "not found") ||
			strings.Contains(lower, "11128")
	}
	return strings.Contains(ue.Message, "11128")
}

// buildMessagesBody renders the Anthropic wire for the Messages lane. The
// preprocessing mirrors the chat builder: empty-name tool calls AND their
// paired tool results are pruned (11133 poison), and thinking blocks ride
// only on assistant tool turns.
func (d *Driver) buildMessagesBody(call driver.ChatCall, creds *Creds) ([]byte, error) {
	req := call.Request
	badIDs := map[string]bool{}
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && strings.TrimSpace(tc.Name) == "" {
				badIDs[tc.ID] = true
			}
		}
	}
	var system []map[string]any
	var msgs []map[string]any
	for _, m := range req.Messages {
		if m.Role == ir.RoleSystem {
			for _, p := range m.Content {
				if p.Kind == "text" && p.Text != "" {
					system = append(system, map[string]any{"type": "text", "text": p.Text})
				}
			}
			continue
		}
		if m.Role == ir.RoleTool {
			if badIDs[m.ToolCallID] {
				continue
			}
			var blocks []map[string]any
			for _, p := range m.Content {
				switch p.Kind {
				case "text":
					if p.Text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
					}
				case "image":
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "base64", "media_type": p.ImageMIME, "data": p.ImageB64,
						},
					})
				}
			}
			var result any = ""
			if len(blocks) == 1 {
				result = blocks[0]
			} else if len(blocks) > 1 {
				result = blocks
			}
			msgs = append(msgs, map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": result}},
			})
			continue
		}
		var parts []map[string]any
		// Thinking replay: same rule as the chat lane — only on assistant
		// tool turns, before text and tool_use.
		if m.Role == ir.RoleAssistant && len(m.ToolCalls) > 0 {
			for _, th := range m.Thinking {
				if th.Text == "" && th.Signature == "" {
					continue
				}
				parts = append(parts, map[string]any{
					"type": "thinking", "thinking": th.Text, "signature": th.Signature,
				})
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
		for _, tc := range m.ToolCalls {
			if badIDs[tc.ID] {
				continue
			}
			var input any = map[string]any{}
			if raw := strings.TrimSpace(tc.Arguments); raw != "" && json.Valid([]byte(raw)) {
				input = json.RawMessage(raw) // BP-128: numbers stay numbers
			}
			parts = append(parts, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input})
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"type": "text", "text": ""})
		}
		msgs = append(msgs, map[string]any{"role": string(m.Role), "content": parts})
	}
	if len(system) > 0 {
		system[len(system)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	// cache_control on the last content block of the last non-system message.
	for i := len(msgs) - 1; i >= 0; i-- {
		if content, ok := msgs[i]["content"].([]map[string]any); ok && len(content) > 0 {
			content[len(content)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
			break
		}
	}
	body := map[string]any{
		"model":      call.UpstreamModel,
		"messages":   msgs,
		"stream":     true,
		"max_tokens": map[bool]int{true: req.MaxTokens, false: 8192}[req.MaxTokens > 0],
	}
	if len(system) > 0 {
		body["system"] = system
	}
	if gear, ok := messagesEffort(req.RequestedEffort); ok {
		body["output_config"] = map[string]any{"effort": gear}
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.Thinking.BudgetTokens > 0 && req.RequestedEffort == "" {
		// An explicit Anthropic thinking block rides as itself when the client
		// did not name a gear; a gear goes through output_config instead.
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": req.Thinking.BudgetTokens}
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tool := map[string]any{"name": t.Name}
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
	if tc := wbAnthropicToolChoice(req.ToolChoice, req.ToolChoiceName, req.DisableParallel); tc != nil {
		body["tool_choice"] = tc
	}
	return json.Marshal(body)
}

// wbAnthropicToolChoice renders the IR's portable choice vocabulary on the
// Anthropic wire (mirrors the zcode driver's BP-69 fix — forced/named calls
// were silently ignored without it).
func wbAnthropicToolChoice(choice any, name string, disableParallel bool) map[string]any {
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

// tryMessages runs one Messages-lane request. ok=false means "fall back to
// the chat lane" (route missing, channel check, build failure); a genuine
// upstream error comes back as err for the pool to act on.
func (d *Driver) tryMessages(ctx context.Context, call driver.ChatCall, creds *Creds) (*http.Response, bool, error) {
	body, err := d.buildMessagesBody(call, creds)
	if err != nil {
		return nil, false, nil
	}
	resp, err := d.postChat(ctx, creds, call, body, messagesPath)
	if err != nil {
		if ue, ok := err.(*uperr.Error); ok {
			// A 401 is worth one proven retry, exactly like the chat lane.
			if ue.Class == uperr.ClassAuth && ue.Status == 401 {
				if rerr := d.refresh(ctx, call.Account, true); rerr == nil {
					if fresh, perr := ParseCreds(call.Account.Credentials()); perr == nil &&
						fresh.AccessToken != "" && fresh.AccessToken != creds.AccessToken {
						if body2, berr := d.buildMessagesBody(call, fresh); berr == nil {
							log.Printf("workbuddy: 401 on /v1/messages; renewed credentials once and resending")
							resp, err = d.postChat(ctx, fresh, call, body2, messagesPath)
						}
					}
				}
			}
		}
		if err != nil {
			if ue, ok := err.(*uperr.Error); ok && messagesRouteDead(ue) {
				d.messagesAllowed.Store(false)
				log.Printf("workbuddy: /v1/messages refused (%.120s); claude models degrade to the chat lane for this process", ue.Message)
				return nil, false, nil
			}
			return nil, false, err
		}
	}
	return resp, true, nil
}

// parseMessagesSSE consumes the Anthropic-shaped Messages stream into IR
// events (same wire the zcode channel speaks).
func (d *Driver) parseMessagesSSE(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: time.Now().Unix(), Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		tools      = map[int]*ir.ToolCall{}
		toolOrder  []int
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if code, msg, ok := detectBusinessError([]byte(payload)); ok && code != 0 {
			return nil, classifyBusiness(code, msg, call.UpstreamModel)
		}
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *struct {
					InputTokens int `json:"input_tokens"`
					// Anthropic-wire prefix cache accounting (BP-53): absent
					// must not read as a reported zero.
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Index        int `json:"index"`
			ContentBlock *struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Signature string `json:"signature"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
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
				// is the FULL prompt, cache included. Anthropic wire
				// input_tokens excludes the cache split — fold it back (BP-56).
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
				if err := emit(ir.StreamEvent{Kind: ir.EvThinkingSignature, Delta: ev.ContentBlock.Signature}); err != nil {
					return nil, err
				}
			}
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				tc := &ir.ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
				tools[ev.Index] = tc
				toolOrder = append(toolOrder, ev.Index)
				if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: ev.Index, ToolCall: tc}); err != nil {
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
				if ev.Delta.Signature != "" {
					if err := emit(ir.StreamEvent{Kind: ir.EvThinkingSignature, Delta: ev.Delta.Signature}); err != nil {
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
			if code, msg, ok := detectBusinessError([]byte(payload)); ok {
				return nil, classifyBusiness(code, msg, call.UpstreamModel)
			}
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"workbuddy: messages stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy: messages stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy: messages stream ended without message_stop")
	}
	if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolOrder) == 0 && !gotUsage {
		return nil, uperr.New(uperr.ClassTransport, 0,
			"workbuddy: empty completion on the messages lane (quota exhausted or model unavailable)")
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
	out.StopReason = stop
	out.Usage = usage
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	return out, nil
}
