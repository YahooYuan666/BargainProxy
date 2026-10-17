// Package openai implements the OpenAI Chat Completions protocol: request
// parsing into IR, and response/SSE rendering from IR. It also handles the
// OpenAI Responses API surface in responses.go.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/sessionfp"
)

// ChatRequest is the wire shape of POST /v1/chat/completions (subset we
// support: text, images, tools, reasoning effort, stream options).
type ChatRequest struct {
	Model           string         `json:"model"`
	Messages        []WireMessage  `json:"messages"`
	Tools           []WireTool     `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	MaxCompletion   int            `json:"max_completion_tokens,omitempty"`
	Stop            any            `json:"stop,omitempty"` // string | []string
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   *StreamOptions `json:"stream_options,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	ConversationID  string         `json:"conversation_id,omitempty"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitempty"`
	Thinking        *WireThinking  `json:"thinking,omitempty"`
}

// StreamOptions mirrors OpenAI's include_usage.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// WireThinking accepts Anthropic-style thinking blocks some clients send.
type WireThinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// WireMessage is the OpenAI message shape (content string or array).
type WireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []WireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// WireToolCall is an assistant tool call in wire form.
type WireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// WireTool is a tool declaration.
type WireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

// ParseChatRequest converts a wire request into IR.
func ParseChatRequest(body []byte) (*ir.ChatRequest, error) {
	var w ChatRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai: parse request: %w", err)
	}
	req := &ir.ChatRequest{
		Model:           strings.TrimSpace(w.Model),
		Stream:          w.Stream,
		Temperature:     w.Temperature,
		TopP:            w.TopP,
		RequestedEffort: w.ReasoningEffort,
	}
	if w.MaxCompletion > 0 {
		req.MaxTokens = w.MaxCompletion
	} else {
		req.MaxTokens = w.MaxTokens
	}
	switch v := w.Stop.(type) {
	case string:
		if v != "" {
			req.Stop = []string{v}
		}
	case []any:
		for _, s := range v {
			if str, ok := s.(string); ok && str != "" {
				req.Stop = append(req.Stop, str)
			}
		}
	}
	if w.Thinking != nil && (w.Thinking.Type == "enabled" || w.Thinking.BudgetTokens > 0) {
		req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: w.Thinking.BudgetTokens}
	}
	for _, m := range w.Messages {
		msg := ir.Message{Role: ir.Role(m.Role)}
		if msg.Role == "developer" {
			msg.Role = ir.RoleSystem
		}
		parts, err := parseContent(m.Role, m.Content)
		if err != nil {
			return nil, err
		}
		msg.Content = parts
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ir.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		msg.ToolCallID = m.ToolCallID
		msg.ToolName = m.Name
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range w.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		req.Tools = append(req.Tools, ir.ToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	req.ToolChoice = w.ToolChoice
	// Session identity, three-level fallback: explicit ids → cache_control
	// marked part digest → system+first-user prefix digest. Model-independent
	// so switching models inside a conversation keeps the binding.
	req.SessionKey = SessionKeyFrom(w.Metadata, w.ConversationID, w.PromptCacheKey)
	if req.SessionKey == "" {
		req.SessionKey = cacheControlKey(w.Messages)
	}
	if req.SessionKey == "" {
		req.SessionKey = sessionfp.FromMessages(req.Messages)
	}
	req.Raw = map[string]any{
		"include_usage": w.StreamOptions != nil && w.StreamOptions.IncludeUsage,
	}
	return req, nil
}

// SessionKeyFrom extracts an explicit session identity from OpenAI-side
// fields: metadata.user_id JSON (Claude-Code style), conversation_id, or
// prompt_cache_key. Empty when the client gave nothing stable.
func SessionKeyFrom(metadata map[string]any, conversationID, promptCacheKey string) string {
	if conversationID != "" {
		return "conv:" + conversationID
	}
	if md, ok := metadata["user_id"]; ok {
		switch v := md.(type) {
		case string:
			if sid := parseSessionFromUserID(v); sid != "" {
				return "user:" + sid
			}
		}
	}
	if promptCacheKey != "" {
		return "pck:" + promptCacheKey
	}
	return ""
}

func parseSessionFromUserID(raw string) string {
	var parsed struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed.SessionID != "" {
		return parsed.SessionID
	}
	return ""
}

// cacheControlKey scans message content parts in document order for the
// first part carrying a cache_control marker and returns its digest. The
// first client-placed cache breakpoint is the stable prompt anchor; later
// breakpoints move with the conversation tail every turn. Empty when no
// part is marked (most OpenAI-native clients never mark).
func cacheControlKey(msgs []WireMessage) string {
	for _, m := range msgs {
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue // plain-string content cannot carry markers
		}
		for _, p := range parts {
			if v, ok := p["cache_control"]; !ok || v == nil {
				continue
			}
			if t, ok := p["text"].(string); ok && t != "" {
				return sessionfp.CacheControl(t)
			}
		}
	}
	return ""
}

func parseContent(role string, raw json.RawMessage) ([]ir.ContentPart, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// String form.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ir.ContentPart{{Kind: "text", Text: s}}, nil
	}
	// Array form.
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("openai: message content: %w", err)
	}
	var out []ir.ContentPart
	for _, item := range arr {
		typ, _ := item["type"].(string)
		switch typ {
		case "text", "input_text", "output_text":
			if t, ok := item["text"].(string); ok && t != "" {
				out = append(out, ir.ContentPart{Kind: "text", Text: t})
			}
		case "image_url":
			iu, _ := item["image_url"].(map[string]any)
			url, _ := iu["url"].(string)
			if url == "" {
				continue
			}
			if mime, b64, ok := ParseDataURL(url); ok {
				out = append(out, ir.ContentPart{Kind: "image", ImageMIME: mime, ImageB64: b64})
			}
			// External http(s) image URLs are rejected upstream-side by
			// policy (no server-side fetching). We keep them as a marker so
			// the gateway can produce a clear protocol error.
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				out = append(out, ir.ContentPart{Kind: "image_url_unsupported", Text: url})
			}
		}
	}
	return out, nil
}

// ParseDataURL splits a data: URL into (mime, base64).
func ParseDataURL(u string) (string, string, bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(u, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, data := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mime := strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return mime, data, true
}

// --- response rendering ---

// ChatCompletion is the non-streamed response body.
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion choice.
type Choice struct {
	Index        int          `json:"index"`
	Message      *RespMessage `json:"message,omitempty"`
	Delta        *RespDelta   `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// RespMessage is the assistant message in non-streamed form.
type RespMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []RespToolCall `json:"tool_calls,omitempty"`
}

// RespDelta is the streaming delta shape.
type RespDelta struct {
	Role             string         `json:"role,omitempty"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []RespToolCall `json:"tool_calls,omitempty"`
}

// RespToolCall is the OpenAI tool_call render shape.
type RespToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// Usage is the OpenAI usage block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// UsageFromIR converts normalized usage.
func UsageFromIR(u ir.Usage) *Usage {
	return &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// RenderCompletion renders a full ChatCompletion.
func RenderCompletion(id, model string, created int64, resp *ir.ChatResponse) *ChatCompletion {
	finish := string(resp.StopReason)
	msg := &RespMessage{Role: "assistant", Content: textOf(resp.Message.Content)}
	// Aggregated reasoning surfaces as reasoning_content — DeepSeek-style
	// clients expect it in non-streamed completions too (BP-41).
	if resp.Thinking != "" {
		msg.ReasoningContent = resp.Thinking
	}
	for _, tc := range resp.Message.ToolCalls {
		var rtc RespToolCall
		rtc.ID = tc.ID
		rtc.Type = "function"
		rtc.Function.Name = tc.Name
		rtc.Function.Arguments = tc.Arguments
		msg.ToolCalls = append(msg.ToolCalls, rtc)
	}
	return &ChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   UsageFromIR(resp.Usage),
	}
}

// Chunk is the streamed response shape.
type Chunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// RenderChunk renders one streaming chunk.
func RenderChunk(id, model string, created int64, delta *RespDelta, finish *string) *Chunk {
	return &Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}

// RenderUsageChunk renders the final usage-only chunk (include_usage).
func RenderUsageChunk(id, model string, created int64, u *Usage) *Chunk {
	return &Chunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []Choice{}, Usage: u}
}

// ErrorBody is the OpenAI error envelope.
type ErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code,omitempty"`
	} `json:"error"`
}

// RenderError builds the OpenAI error envelope.
func RenderError(message, typ string, code any) *ErrorBody {
	e := &ErrorBody{}
	e.Error.Message = message
	e.Error.Type = typ
	e.Error.Code = code
	return e
}

func textOf(parts []ir.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
