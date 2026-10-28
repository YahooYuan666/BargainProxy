// Package responses implements the OpenAI Responses API surface: parsing
// `input`/`instructions` into IR, and rendering both the non-streamed
// response object and the evented SSE state machine
// (response.created -> in_progress -> output_item.added ->
// content_part.added -> output_text.delta* -> output_text.done ->
// output_item.done -> response.completed).
package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/sessionfp"
)

// Request is the wire shape of POST /v1/responses (the subset we support).
type Request struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"` // string | []item
	Instructions string          `json:"instructions,omitempty"`
	Tools        []WireTool      `json:"tools,omitempty"`
	ToolChoice   any             `json:"tool_choice,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
	MaxOutput    int             `json:"max_output_tokens,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	Reasoning    *WireReasoning  `json:"reasoning,omitempty"`
	Metadata     map[string]any  `json:"metadata,omitempty"`
	PreviousID   string          `json:"previous_response_id,omitempty"`
	Store        *bool           `json:"store,omitempty"`
}

// WireReasoning carries the Responses reasoning effort.
type WireReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// WireTool: Responses uses flat {"type":"function","name":...,"parameters":...}.
type WireTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ParseRequest converts a Responses request into IR.
func ParseRequest(body []byte) (*ir.ChatRequest, error) {
	var w Request
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("responses: parse request: %w", err)
	}
	// The gateway is stateless: it cannot reconstruct the omitted prefix of a
	// previous_response_id chain. Silently ignoring the id would drop context
	// and produce wrong answers, so fail loudly and ask for the full history.
	if strings.TrimSpace(w.PreviousID) != "" {
		return nil, fmt.Errorf("responses: previous_response_id is not supported (stateless gateway); send the full input history")
	}
	req := &ir.ChatRequest{
		Model:       strings.TrimSpace(w.Model),
		Stream:      w.Stream,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		MaxTokens:   w.MaxOutput,
	}
	if w.Reasoning != nil && w.Reasoning.Effort != "" {
		req.RequestedEffort = w.Reasoning.Effort
	}
	if w.Instructions != "" {
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: w.Instructions}}})
	}
	msgs, err := parseInput(w.Input)
	if err != nil {
		return nil, err
	}
	req.Messages = append(req.Messages, msgs...)
	for _, t := range w.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		req.Tools = append(req.Tools, ir.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	req.ToolChoice, req.ToolChoiceName, req.DisableParallel = ir.NormalizeToolChoice(w.ToolChoice)
	// Session identity, three-level fallback: explicit metadata ids →
	// cache_control marked part digest → system/instructions + first-user
	// prefix digest. Model-independent, so switching models mid-conversation
	// keeps the binding.
	req.SessionKey = sessionKeyFrom(w.Metadata)
	if req.SessionKey == "" {
		req.SessionKey = cacheControlKey(w.Input)
	}
	if req.SessionKey == "" {
		req.SessionKey = sessionfp.FromMessages(req.Messages)
	}
	return req, nil
}

// cacheControlKey scans input content parts for the first cache_control
// marker and returns its digest (level-2 session identity). Empty when no
// part is marked.
func cacheControlKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	for _, item := range items {
		parts, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if v, ok := m["cache_control"]; !ok || v == nil {
				continue
			}
			if t, ok := m["text"].(string); ok && t != "" {
				return sessionfp.CacheControl(t)
			}
		}
	}
	return ""
}

func sessionKeyFrom(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	for _, key := range []string{"conversation_id", "session_id"} {
		if v, ok := metadata[key].(string); ok && v != "" {
			return "meta:" + v
		}
	}
	if raw, ok := metadata["user_id"].(string); ok && raw != "" {
		var parsed struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed.SessionID != "" {
			return "user:" + parsed.SessionID
		}
	}
	return ""
}

// parseInput handles input as a plain string or an item array. Item forms:
// {role, content} messages (content: string or parts), function_call,
// function_call_output.
func parseInput(raw json.RawMessage) ([]ir.Message, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: s}}}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses: input: %w", err)
	}
	var out []ir.Message
	for _, item := range items {
		typ, _ := item["type"].(string)
		switch typ {
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			out = append(out, ir.Message{
				Role:      ir.RoleAssistant,
				ToolCalls: []ir.ToolCall{{ID: callID, Name: name, Arguments: args}},
			})
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			var text string
			switch ov := item["output"].(type) {
			case string:
				text = ov
			default:
				if b, err := json.Marshal(ov); err == nil {
					text = string(b)
				}
			}
			out = append(out, ir.Message{Role: ir.RoleTool, ToolCallID: callID, Content: []ir.ContentPart{{Kind: "text", Text: text}}})
		case "reasoning", "item_reference":
			// Reasoning items carry provider-encrypted state that only the
			// issuing Responses backend can replay; the gateway has no
			// equivalent. Dropping is safe — treating them as messages would
			// inject an empty user turn and make upstreams reject the request
			// (BP-69).
			continue
		default:
			role, _ := item["role"].(string)
			parts, err := parseContent(item["content"])
			if err != nil {
				return nil, err
			}
			// Unknown item shapes must never become empty messages. A message
			// with no usable content is not a turn the upstream can use.
			if len(parts) == 0 {
				continue
			}
			r := ir.RoleUser
			if role == "assistant" {
				r = ir.RoleAssistant
			} else if role == "system" || role == "developer" {
				r = ir.RoleSystem
			}
			out = append(out, ir.Message{Role: r, Content: parts})
		}
	}
	return out, nil
}

func parseContent(content any) ([]ir.ContentPart, error) {
	switch cv := content.(type) {
	case string:
		if cv == "" {
			return nil, nil
		}
		return []ir.ContentPart{{Kind: "text", Text: cv}}, nil
	case []any:
		var parts []ir.ContentPart
		for _, item := range cv {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text", "text":
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, ir.ContentPart{Kind: "text", Text: t})
				}
			case "input_image", "image_url":
				if u, _ := m["image_url"].(string); strings.HasPrefix(u, "data:") {
					if meta, data, ok := splitDataURL(u); ok {
						parts = append(parts, ir.ContentPart{Kind: "image", ImageMIME: meta, ImageB64: data})
					}
				} else if u, _ := m["image_url"].(string); u != "" {
					parts = append(parts, ir.ContentPart{Kind: "image_url_unsupported", Text: u})
				}
			}
		}
		return parts, nil
	default:
		return nil, nil
	}
}

func splitDataURL(u string) (string, string, bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(u, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, data := rest[:comma], rest[comma+1:]
	meta = strings.TrimSuffix(meta, ";base64")
	if meta == "" {
		meta = "application/octet-stream"
	}
	return meta, data, true
}

// --- rendering ---

// ResponseObject is the non-streamed response body.
type ResponseObject struct {
	ID         string             `json:"id"`
	Object     string             `json:"object"`
	CreatedAt  int64              `json:"created_at"`
	Status     string             `json:"status"`
	Model      string             `json:"model"`
	Output     []any              `json:"output"`
	OutputText string             `json:"output_text,omitempty"`
	Usage      *Usage             `json:"usage,omitempty"`
	// IncompleteDetails (BP-181) names why a turn ended incomplete.
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
}

// IncompleteDetails is the Responses API's standard truncation reason shape.
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// Usage is the Responses usage shape.
type Usage struct {
	InputTokens         int                  `json:"input_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                  `json:"output_tokens"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                  `json:"total_tokens"`
}

// InputTokensDetails carries the prefix-cache subset. ZCode reads
// input_tokens_details.cached_tokens; omitting it makes every hit look like a
// miss on the client side (BP-69).
type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputTokensDetails carries the reasoning-token subset.
type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func usageFromIR(u ir.Usage) *Usage {
	out := &Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.InputTokens + u.OutputTokens,
	}
	if u.CacheKnown {
		out.InputTokensDetails = &InputTokensDetails{CachedTokens: u.CachedInputTokens}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &OutputTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// Render builds a non-streamed response object.
func Render(id, model string, createdAt int64, resp *ir.ChatResponse, outputText string) *ResponseObject {
	out := &ResponseObject{
		ID:        id,
		Object:    "response",
		CreatedAt: createdAt,
		Status:    "completed",
		Model:     model,
	}
	// BP-181: a length-capped turn reports incomplete/max_output_tokens
	// instead of a completed status that claims the answer is whole.
	if resp != nil && resp.StopReason == ir.StopLength {
		out.Status = "incomplete"
		out.IncompleteDetails = &IncompleteDetails{Reason: "max_output_tokens"}
	}
	msgItem := map[string]any{
		"type":   "message",
		"id":     "msg_" + id,
		"status": "completed",
		"role":   "assistant",
		"content": []map[string]any{
			{"type": "output_text", "text": outputText, "annotations": []any{}},
		},
	}
	out.Output = append(out.Output, msgItem)
	for _, tc := range resp.Message.ToolCalls {
		out.Output = append(out.Output, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + tc.ID,
			"call_id":   tc.ID,
			"name":      tc.Name,
			"arguments": tc.Arguments,
			"status":    "completed",
		})
	}
	out.OutputText = outputText
	out.Usage = usageFromIR(resp.Usage)
	return out
}

// ErrorBody is the Responses-compatible error envelope (same as OpenAI).
type ErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code,omitempty"`
	} `json:"error"`
}

// RenderError builds the error envelope.
func RenderError(kind, message string) *ErrorBody {
	e := &ErrorBody{}
	e.Error.Type = kind
	e.Error.Message = message
	return e
}
