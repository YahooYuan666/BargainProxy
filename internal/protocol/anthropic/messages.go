// Package anthropic implements the Anthropic Messages protocol: request
// parsing into IR and response/SSE rendering from IR, including the strict
// message-alternation preprocessing and the count_tokens endpoint contract
// (best-effort estimate with a safe fallback).
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/sessionfp"
)

// Request is the wire shape of POST /v1/messages.
type Request struct {
	Model         string          `json:"model"`
	Messages      []WireMessage   `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"` // string | []block
	Tools         []WireTool      `json:"tools,omitempty"`
	ToolChoice    map[string]any  `json:"tool_choice,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      *WireThinking   `json:"thinking,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`
}

// WireThinking is the Anthropic extended-thinking block.
type WireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// WireMessage is one Anthropic message (content blocks array).
type WireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []block
}

// WireTool is an Anthropic tool declaration (input_schema, no wrapper).
type WireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ParseMessagesRequest converts a wire request into IR, applying the
// Anthropic-specific normalization (system extraction, adjacent same-role
// merging, tool_result blocks becoming tool messages).
func ParseMessagesRequest(body []byte) (*ir.ChatRequest, error) {
	var w Request
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("anthropic: parse request: %w", err)
	}
	req := &ir.ChatRequest{
		Model:       strings.TrimSpace(w.Model),
		Stream:      w.Stream,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		TopK:        w.TopK,
		MaxTokens:   w.MaxTokens,
		Stop:        w.StopSequences,
	}
	if w.Thinking != nil && (w.Thinking.Type == "enabled" || w.Thinking.Type == "adaptive") {
		// Anthropic semantics: thinking enabled means the client expects the
		// reasoning blocks back (Include), and channels that only speak gear
		// names get a level derived from the budget (BP-41).
		req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: w.Thinking.BudgetTokens, Include: true}
		if lvl := ir.LevelFromBudget(w.Thinking.BudgetTokens); lvl != "" {
			req.Thinking.Level = lvl
			req.RequestedEffort = lvl
		} else {
			req.RequestedEffort = "high"
		}
	}

	// System: string or blocks -> one system message at the head.
	if len(w.System) > 0 {
		text := systemText(w.System)
		if text != "" {
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: text}}})
		}
	}

	for _, m := range w.Messages {
		msgs, err := parseMessage(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msgs...)
	}
	// Merge adjacent same-role messages (Anthropic requires strict
	// alternation; clients that diverged from it are normalized here so
	// downstream channels never see double-user turns).
	req.Messages = mergeAdjacent(req.Messages)
	capToolResultImages(req.Messages, maxToolResultImages)
	// Anthropic's tool_result block carries only tool_use_id — no name. Every
	// downstream channel that re-emits Gemini functionResponse (antigravity)
	// or a tool name needs it, and without it the driver fell back to the
	// literal "tool", producing a result that answers a call named "tool"
	// while the call was named e.g. "Bash". The upstream rejects that pairing
	// with a bare "Request contains an invalid argument" (field 2026-10-25:
	// the subagent request, dumped at 00:02, carried
	// functionCall.name=Bash next to functionResponse.name=tool).
	fillToolResultNames(req.Messages)

	for _, t := range w.Tools {
		req.Tools = append(req.Tools, ir.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	if len(w.ToolChoice) > 0 {
		req.ToolChoice, req.ToolChoiceName, req.DisableParallel = ir.NormalizeToolChoice(map[string]any(w.ToolChoice))
	}
	// Session identity, three-level fallback: explicit metadata id →
	// cache_control-marked block digest → system+first-user prefix digest.
	// The fingerprint is model-independent, so switching models inside a
	// conversation keeps the binding.
	req.SessionKey = SessionKeyFrom(w.Metadata)
	if req.SessionKey == "" {
		req.SessionKey = CacheControlKey(w.System, w.Messages)
	}
	if req.SessionKey == "" {
		req.SessionKey = sessionfp.FromMessages(req.Messages)
	}
	return req, nil
}

// SessionKeyFrom extracts the explicit session id from Anthropic metadata
// (metadata.user_id is a JSON string containing session_id for Claude Code).
func SessionKeyFrom(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata["user_id"].(string)
	if !ok || raw == "" {
		return ""
	}
	var parsed struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed.SessionID != "" {
		return "user:" + parsed.SessionID
	}
	return ""
}

// CacheControlKey scans content blocks in document order (system first, then
// messages) for the first cache_control-marked text block and returns its
// digest. The first client-placed cache breakpoint is the conversation's
// stable prompt anchor; later breakpoints move with the tail every turn.
// Empty when no block is marked.
func CacheControlKey(system json.RawMessage, msgs []WireMessage) string {
	if t := firstMarkedBlockText(system); t != "" {
		return sessionfp.CacheControl(t)
	}
	for _, m := range msgs {
		if t := firstMarkedBlockText(m.Content); t != "" {
			return sessionfp.CacheControl(t)
		}
	}
	return ""
}

// firstMarkedBlockText returns the text of the first block carrying a
// non-nil cache_control marker in a content field (string | []block).
func firstMarkedBlockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "" // plain-string content cannot carry markers
	}
	for _, b := range blocks {
		if v, ok := b["cache_control"]; !ok || v == nil {
			continue
		}
		if t, ok := b["text"].(string); ok && t != "" {
			return t
		}
	}
	return ""
}

func systemText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseMessage parses one Anthropic message into one or more IR messages
// (a content array with tool_result blocks yields trailing tool messages).
func parseMessage(m WireMessage) ([]ir.Message, error) {
	// String content shortcut.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		role := ir.Role(m.Role)
		if role != ir.RoleUser && role != ir.RoleAssistant {
			role = ir.RoleUser
		}
		return []ir.Message{{Role: role, Content: []ir.ContentPart{{Kind: "text", Text: s}}}}, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("anthropic: message content: %w", err)
	}
	var (
		main     ir.Message
		toolMsgs []ir.Message
	)
	main.Role = ir.Role(m.Role)
	if main.Role != ir.RoleUser && main.Role != ir.RoleAssistant {
		main.Role = ir.RoleUser
	}
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "text":
			if t, _ := b["text"].(string); t != "" {
				main.Content = append(main.Content, ir.ContentPart{Kind: "text", Text: t})
			}
		case "thinking":
			// Preserve assistant reasoning for upstreams that can replay it.
			// Kept out of Content so OpenAI-shaped drivers never treat it as
			// body text; the zcode driver decides whether the signature makes
			// the block replayable (BP-69).
			if main.Role == ir.RoleAssistant {
				text, _ := b["thinking"].(string)
				sig, _ := b["signature"].(string)
				if text != "" || sig != "" {
					main.Thinking = append(main.Thinking, ir.ThinkingBlock{Text: text, Signature: sig})
				}
			}
		case "redacted_thinking":
			// Provider-encrypted reasoning (Anthropic-only): not replayable
			// through a foreign gateway, keep dropping.
		case "image":
			if src, ok := b["source"].(map[string]any); ok {
				if srcType, _ := src["type"].(string); srcType == "base64" {
					mime, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					main.Content = append(main.Content, ir.ContentPart{Kind: "image", ImageMIME: mime, ImageB64: data})
				} else if srcType == "url" {
					u, _ := src["url"].(string)
					main.Content = append(main.Content, ir.ContentPart{Kind: "image_url_unsupported", Text: u})
				}
			}
		case "tool_use":
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			inputRaw, _ := json.Marshal(b["input"])
			main.ToolCalls = append(main.ToolCalls, ir.ToolCall{ID: id, Name: name, Arguments: string(inputRaw)})
		case "tool_result":
			id, _ := b["tool_use_id"].(string)
			var resultParts []ir.ContentPart
			switch cv := b["content"].(type) {
			case string:
				if cv != "" {
					resultParts = append(resultParts, ir.ContentPart{Kind: "text", Text: cv})
				}
			case []any:
				for _, item := range cv {
					sub, ok := item.(map[string]any)
					if !ok {
						continue
					}
					switch typ, _ := sub["type"].(string); typ {
					case "text":
						if t, _ := sub["text"].(string); t != "" {
							resultParts = append(resultParts, ir.ContentPart{Kind: "text", Text: t})
						}
					case "image":
						if src, ok := sub["source"].(map[string]any); ok {
							switch srcType, _ := src["type"].(string); srcType {
							case "base64":
								mime, _ := src["media_type"].(string)
								data, _ := src["data"].(string)
								if data != "" {
									resultParts = append(resultParts, ir.ContentPart{Kind: "image", ImageMIME: mime, ImageB64: data})
								}
							case "url":
								if u, _ := src["url"].(string); u != "" {
									resultParts = append(resultParts, ir.ContentPart{Kind: "image_url_unsupported", Text: u})
								}
							}
						}
					}
				}
			}
			if len(resultParts) == 0 {
				resultParts = []ir.ContentPart{{Kind: "text", Text: ""}}
			}
			toolMsgs = append(toolMsgs, ir.Message{
				Role:       ir.RoleTool,
				ToolCallID: id,
				Content:    resultParts,
			})
		}
	}
	var out []ir.Message
	if len(main.Content) > 0 || len(main.ToolCalls) > 0 {
		out = append(out, main)
	}
	out = append(out, toolMsgs...)
	return out, nil
}

// maxToolResultImages bounds how many tool-result images one request may carry.
// Images are expensive and a single tool round can return many; keeping the
// most recent five matches AIClient2API's #718 budget while still giving the
// model the latest screenshots (BP-70).
const maxToolResultImages = 5

// fillToolResultNames copies each assistant tool call's name onto the tool
// message that answers it (BP-116). Anthropic's wire carries only the id on a
// tool_result, so an IR tool message built from it has no ToolName; channels
// that speak Gemini's functionResponse need the name on BOTH sides and, with
// an empty one, the driver substituted the literal "tool" — a result that
// answers nothing, which the upstream rejects with an unhelpful
// "invalid argument".
//
// Matching is by call id, so parallel calls and out-of-order results pair
// correctly. A result whose call is not in this history keeps its empty name
// (the driver's own fallback then applies, and that case really is unknown).
func fillToolResultNames(msgs []ir.Message) {
	names := map[string]string{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.ID != "" && c.Name != "" {
				names[c.ID] = c.Name
			}
		}
	}
	if len(names) == 0 {
		return
	}
	for i := range msgs {
		if msgs[i].Role != ir.RoleTool || msgs[i].ToolName != "" {
			continue
		}
		if n, ok := names[msgs[i].ToolCallID]; ok {
			msgs[i].ToolName = n
		}
	}
}

// capToolResultImages keeps only the most recent max images across all tool
// messages. Text blocks stay; only image parts are dropped.
func capToolResultImages(msgs []ir.Message, max int) {
	if max <= 0 {
		return
	}
	kept := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != ir.RoleTool {
			continue
		}
		parts := msgs[i].Content
		out := make([]ir.ContentPart, 0, len(parts))
		for j := len(parts) - 1; j >= 0; j-- {
			p := parts[j]
			if p.Kind == "image" {
				if kept >= max {
					continue
				}
				kept++
			}
			out = append(out, p)
		}
		for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
			out[l], out[r] = out[r], out[l]
		}
		msgs[i].Content = out
	}
}

// mergeAdjacent merges consecutive messages with the same role.
func mergeAdjacent(in []ir.Message) []ir.Message {
	var out []ir.Message
	for _, m := range in {
		if len(out) > 0 {
			last := &out[len(out)-1]
			if last.Role == m.Role && last.Role != ir.RoleTool && m.Role != ir.RoleTool {
				last.Content = append(last.Content, m.Content...)
				last.ToolCalls = append(last.ToolCalls, m.ToolCalls...)
				last.Thinking = append(last.Thinking, m.Thinking...)
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// --- response rendering ---

// MessageResponse is the non-streamed /v1/messages response.
type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// ContentBlock is one response content block.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// Signature rides on thinking blocks. Even when unknown it must be
	// present on the wire: strict clients (Claude Code, Grok Build) reject a
	// thinking block with "missing field `signature`" — reference: sub2api
	// apicompat MarshalJSON + thinking_signature_wire_test.go (2026-10-16).
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
}

// MarshalJSON pins the Anthropic wire contract for strict clients: thinking
// blocks ALWAYS carry `thinking` and `signature` keys (empty signature is how
// Anthropic itself emits content_block_start), and text blocks always carry
// `text`. Other block types marshal unchanged. Reference: sub2api
// apicompat/types.go.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type alias ContentBlock
	switch b.Type {
	case "thinking":
		return json.Marshal(struct {
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			alias
		}{Thinking: b.Thinking, Signature: b.Signature, alias: alias(b)})
	case "text":
		return json.Marshal(struct {
			Text string `json:"text"`
			alias
		}{Text: b.Text, alias: alias(b)})
	default:
		return json.Marshal(alias(b))
	}
}

// Usage is the Anthropic usage shape. The protocol counts the prompt as
// input_tokens (what missed the prefix cache) + cache_creation (entries
// written) + cache_read (entries served from cache); clients sum the three
// into the prompt size, so the split has to stay exact. ir.Usage carries the
// OpenAI-style total with an optional cached subset instead — convert with
// UsageFromIR rather than copying the total into input_tokens (BP-56).
type Usage struct {
	InputTokens int `json:"input_tokens"`
	// Pointers: a cache account the upstream never reported must stay absent
	// (BP-53 — "unknown" and "reported 0" are different facts), while a
	// reported 0 is still emitted explicitly.
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int  `json:"output_tokens"`
}

// UsageFromIR converts IR accounting into the Anthropic split: input_tokens
// gets the uncached remainder, cache_read_input_tokens the cached subset
// (only when the upstream reported cache accounting at all). cache_creation
// stays absent — no upstream of ours reports a separate write count, and the
// client-side total (input + creation + read) is preserved without it.
func UsageFromIR(u ir.Usage) Usage {
	out := Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if !u.CacheKnown {
		return out
	}
	cached := u.CachedInputTokens
	if cached < 0 {
		cached = 0
	}
	if cached > out.InputTokens {
		cached = out.InputTokens // upstream noise: never emit a negative prompt
	}
	out.InputTokens -= cached
	read := cached
	out.CacheReadInputTokens = &read
	return out
}

// RenderMessage renders a full non-streamed response.
func RenderMessage(id, model string, resp *ir.ChatResponse) *MessageResponse {
	reason := mapStop(resp.StopReason)
	out := &MessageResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		StopReason: &reason,
		Usage:      UsageFromIR(resp.Usage),
	}
	text := textOf(resp.Message.Content)
	// Thinking first (Anthropic block order): the aggregated reasoning from
	// the upstream is rendered as a thinking block. The signature is forwarded
	// only when the upstream issued one; fabricating it would fail verification
	// on replay (BP-41/BP-69).
	if resp.Thinking != "" {
		out.Content = append(out.Content, ContentBlock{
			Type: "thinking", Thinking: resp.Thinking, Signature: resp.ThinkingSignature,
		})
	}
	if text != "" {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range resp.Message.ToolCalls {
		var input any
		if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
			input = map[string]any{}
		}
		out.Content = append(out.Content, ContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
	}
	if len(out.Content) == 0 {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: ""})
	}
	return out
}

func mapStop(r ir.StopReason) string {
	switch r {
	case ir.StopEnd:
		return "end_turn"
	case ir.StopLength:
		return "max_tokens"
	case ir.StopToolUse:
		return "tool_use"
	case ir.StopContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// ErrorBody is the Anthropic error envelope.
type ErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// RenderError builds the Anthropic error envelope.
func RenderError(kind, message string) *ErrorBody {
	e := &ErrorBody{Type: "error"}
	e.Error.Type = kind
	e.Error.Message = message
	return e
}

// CountTokensResponse is the count_tokens response shape.
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// EstimateTokens is the best-effort token estimate: characters/4 adjusted
// for CJK (roughly 1 token per CJK char). Accuracy is not promised; the
// contract (and AIClient2API parity) is "never block the client".
func EstimateTokens(req *ir.ChatRequest) int {
	chars, cjk := 0, 0
	count := func(s string) {
		for _, r := range s {
			chars++
			if r >= 0x4E00 && r <= 0x9FFF {
				cjk++
			}
		}
	}
	for _, m := range req.Messages {
		for _, p := range m.Content {
			count(p.Text)
		}
		for _, th := range m.Thinking {
			count(th.Text)
		}
		for _, tc := range m.ToolCalls {
			count(tc.Arguments)
			count(tc.Name)
		}
	}
	for _, t := range req.Tools {
		count(t.Name)
		count(t.Description)
	}
	estimate := cjk + (chars-cjk)/4
	if estimate < 1 {
		estimate = 1
	}
	return estimate
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
