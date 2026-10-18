// Package geminiv1 implements the native Gemini v1beta generateContent API
// surface: contents/systemInstruction parsing into IR and Gemini-shaped
// responses (streamed as ready-to-pass GenerateContentResponse JSON frames —
// no [DONE] sentinel, per protocol).
package geminiv1

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/sessionfp"
)

// GenerateRequest is the wire shape of :generateContent / :streamGenerateContent.
type GenerateRequest struct {
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []ToolGroup       `json:"tools,omitempty"`
	ToolConfig        *ToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

// Content is a Gemini content block.
type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

// Part is a Gemini part (text / inlineData / functionCall / functionResponse).
type Part struct {
	Text         string          `json:"text,omitempty"`
	Thought      bool            `json:"thought,omitempty"`
	InlineData   *Blob           `json:"inlineData,omitempty"`
	FunctionCall *FunctionCall   `json:"functionCall,omitempty"`
	FunctionResp *FunctionResult `json:"functionResponse,omitempty"`
}

// Blob is inline binary data.
type Blob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

// FunctionCall is a model-issued function call.
type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// FunctionResult is a function response.
type FunctionResult struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// ToolGroup is the Gemini tools wrapper.
type ToolGroup struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations"`
}

// FunctionDeclaration is one function declaration.
type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolConfig carries the calling mode.
type ToolConfig struct {
	FunctionCallingConfig struct {
		Mode string `json:"mode,omitempty"`
	} `json:"functionCallingConfig"`
}

// GenerationConfig carries generation parameters.
type GenerationConfig struct {
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"topP,omitempty"`
	MaxOutputTokens int            `json:"maxOutputTokens,omitempty"`
	StopSequences   []string       `json:"stopSequences,omitempty"`
	ThinkingConfig  map[string]any `json:"thinkingConfig,omitempty"`
}

// ParseRequest converts a Gemini request into IR. sessionKey comes from the
// optional X-Session-Id header (Gemini has no in-body session field); the
// handler passes it in.
func ParseRequest(body []byte, sessionKey string) (*ir.ChatRequest, error) {
	var w GenerateRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("gemini: parse request: %w", err)
	}
	req := &ir.ChatRequest{SessionKey: sessionKey}
	if w.SystemInstruction != nil {
		var text string
		for _, p := range w.SystemInstruction.Parts {
			text += p.Text
		}
		if text != "" {
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: text}}})
		}
	}
	type pendingCall struct {
		id, name string
		used     bool
	}
	var pending []pendingCall
	for ci, c := range w.Contents {
		var msg ir.Message
		if c.Role == "model" {
			msg.Role = ir.RoleAssistant
		} else {
			msg.Role = ir.RoleUser
		}
		var results []ir.Message
		for pi, p := range c.Parts {
			switch {
			case p.Text != "":
				msg.Content = append(msg.Content, ir.ContentPart{Kind: "text", Text: p.Text})
			case p.InlineData != nil:
				msg.Content = append(msg.Content, ir.ContentPart{Kind: "image", ImageMIME: p.InlineData.MIMEType, ImageB64: p.InlineData.Data})
			case p.FunctionCall != nil:
				id := p.FunctionCall.ID
				if id == "" {
					id = fmt.Sprintf("gemini_call_%d_%d", ci, pi)
				}
				args, _ := json.Marshal(p.FunctionCall.Args)
				msg.ToolCalls = append(msg.ToolCalls, ir.ToolCall{ID: id, Name: p.FunctionCall.Name, Arguments: string(args)})
				pending = append(pending, pendingCall{id: id, name: p.FunctionCall.Name})
			case p.FunctionResp != nil:
				id := p.FunctionResp.ID
				for i := range pending {
					if !pending[i].used && ((id != "" && pending[i].id == id) || (id == "" && pending[i].name == p.FunctionResp.Name)) {
						id = pending[i].id
						pending[i].used = true
						break
					}
				}
				payload, _ := json.Marshal(p.FunctionResp.Response)
				results = append(results, ir.Message{Role: ir.RoleTool, ToolCallID: id, ToolName: p.FunctionResp.Name, Content: []ir.ContentPart{{Kind: "text", Text: string(payload)}}})
			}
		}
		req.Messages = append(req.Messages, results...)
		if len(msg.Content) > 0 || len(msg.ToolCalls) > 0 || len(results) == 0 {
			req.Messages = append(req.Messages, msg)
		}
	}
	for _, tg := range w.Tools {
		for _, fd := range tg.FunctionDeclarations {
			req.Tools = append(req.Tools, ir.ToolDef{Name: fd.Name, Description: fd.Description, Parameters: fd.Parameters})
		}
	}
	if w.GenerationConfig != nil {
		gc := w.GenerationConfig
		req.Temperature = gc.Temperature
		req.TopP = gc.TopP
		req.MaxTokens = gc.MaxOutputTokens
		req.Stop = gc.StopSequences
		if gc.ThinkingConfig != nil {
			req.Thinking = &ir.ThinkingConfig{Enabled: true}
			if b, ok := gc.ThinkingConfig["thinkingBudget"].(float64); ok {
				req.Thinking.BudgetTokens = int(b)
			}
			if lvl, ok := gc.ThinkingConfig["thinkingLevel"].(string); ok {
				req.Thinking.Level = strings.ToLower(lvl)
			}
			// Gemini opt-in: thought parts come back only when the client
			// asks for them (BP-41).
			if inc, ok := gc.ThinkingConfig["includeThoughts"].(bool); ok {
				req.Thinking.Include = inc
			}
		}
	}
	if w.ToolConfig != nil {
		req.ToolChoice, req.ToolChoiceName, req.DisableParallel = ir.NormalizeToolChoice(strings.ToLower(w.ToolConfig.FunctionCallingConfig.Mode))
	}
	// Session identity fallback: Gemini has no in-body session field, so a
	// missing X-Session-Id header falls back to the system+first-user prefix
	// digest (level 3), keeping the conversation on one account.
	if req.SessionKey == "" {
		req.SessionKey = sessionfp.FromMessages(req.Messages)
	}
	// The model comes from the URL path (:generateContent), filled by handler.
	return req, nil
}

// --- rendering ---

// Response is the Gemini response envelope (non-streamed).
type Response struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
}

// Candidate is one candidate in the response.
type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason"`
	Index        int     `json:"index"`
}

// UsageMetadata is the Gemini usage shape.
type UsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
}

// Render builds a non-streamed response from IR. includeThoughts mirrors the
// request's includeThoughts: thought parts are only added when the client
// opted in (BP-41).
func Render(model string, resp *ir.ChatResponse, includeThoughts bool) *Response {
	var parts []Part
	if includeThoughts && resp.Thinking != "" {
		parts = append(parts, Part{Text: resp.Thinking, Thought: true})
	}
	if text := textOf(resp.Message.Content); text != "" {
		parts = append(parts, Part{Text: text})
	}
	for _, tc := range resp.Message.ToolCalls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Arguments), &args)
		if args == nil {
			args = map[string]any{}
		}
		parts = append(parts, Part{FunctionCall: &FunctionCall{ID: tc.ID, Name: tc.Name, Args: args}})
	}
	return &Response{
		ModelVersion: model,
		Candidates: []Candidate{{
			Content:      Content{Role: "model", Parts: parts},
			FinishReason: mapFinish(resp.StopReason),
			Index:        0,
		}},
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     resp.Usage.InputTokens,
			CandidatesTokenCount: resp.Usage.OutputTokens,
			TotalTokenCount:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func mapFinish(r ir.StopReason) string {
	switch r {
	case ir.StopLength:
		return "MAX_TOKENS"
	case ir.StopContentFilter:
		return "SAFETY"
	default:
		return "STOP"
	}
}

// ErrorBody is the Google RPC error envelope.
type ErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// RenderError builds a Google RPC error body.
func RenderError(status int, state, message string) *ErrorBody {
	e := &ErrorBody{}
	e.Error.Code = status
	e.Error.Message = message
	e.Error.Status = state
	return e
}

// StreamWriter emits Gemini SSE frames: each event is a full
// GenerateContentResponse JSON (no [DONE] — native SDKs parse each data line
// as a response and stop at stream end).
type StreamWriter struct {
	w       io.Writer
	flusher interface{ Flush() }
	model   string
	// includeThoughts (set from the request) turns EvThinking into thought
	// parts; without it thinking deltas are dropped as before (BP-41).
	includeThoughts bool

	mu        sync.Mutex
	sent      bool
	textBuf   strings.Builder
	usage     *UsageMetadata
	finish    string
	done      bool
	tools     map[int]*geminiToolState
	toolOrder []int
}

type geminiToolState struct {
	id, name string
	args     strings.Builder
}

// NewStreamWriter builds a Gemini stream writer. includeThoughts comes from
// the request's thinkingConfig.includeThoughts.
func NewStreamWriter(w io.Writer, flusher interface{ Flush() }, model string, includeThoughts bool) *StreamWriter {
	return &StreamWriter{w: w, flusher: flusher, model: model, includeThoughts: includeThoughts}
}

func (s *StreamWriter) frame(parts []Part, finish string, usage *UsageMetadata) error {
	resp := Response{
		ModelVersion: s.model,
		Candidates: []Candidate{{
			Content: Content{Role: "model", Parts: parts},
			Index:   0,
		}},
	}
	if finish != "" {
		resp.Candidates[0].FinishReason = finish
	}
	if usage != nil {
		resp.UsageMetadata = usage
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", raw); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// Send routes one IR event.
func (s *StreamWriter) Send(ev ir.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	switch ev.Kind {
	case ir.EvText:
		s.sent = true
		s.textBuf.WriteString(ev.Delta)
		return s.frame([]Part{{Text: ev.Delta}}, "", nil)
	case ir.EvThinking:
		// Thought parts only when the client opted in (includeThoughts);
		// otherwise the delta is dropped as before.
		if !s.includeThoughts || ev.Delta == "" {
			return nil
		}
		return s.frame([]Part{{Text: ev.Delta, Thought: true}}, "", nil)
	case ir.EvToolStart:
		if s.tools == nil {
			s.tools = map[int]*geminiToolState{}
		}
		if _, ok := s.tools[ev.ToolIndex]; !ok {
			s.toolOrder = append(s.toolOrder, ev.ToolIndex)
		}
		st := &geminiToolState{id: ev.ToolCall.ID, name: ev.ToolCall.Name}
		st.args.WriteString(ev.ToolCall.Arguments)
		s.tools[ev.ToolIndex] = st
		return nil
	case ir.EvToolArgs:
		if st := s.tools[ev.ToolIndex]; st != nil {
			st.args.WriteString(ev.Delta)
		}
		return nil
	case ir.EvUsage:
		s.usage = &UsageMetadata{
			PromptTokenCount:     ev.Usage.InputTokens,
			CandidatesTokenCount: ev.Usage.OutputTokens,
			TotalTokenCount:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
		}
		return nil
	case ir.EvDone:
		s.finish = mapFinish(ev.Stop)
		return s.finalize()
	}
	return nil
}

func (s *StreamWriter) finalize() error {
	var parts []Part
	for _, idx := range s.toolOrder {
		st := s.tools[idx]
		raw := st.args.String()
		if raw == "" {
			raw = "{}"
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
			return fmt.Errorf("gemini: incomplete arguments for tool %s", st.name)
		}
		parts = append(parts, Part{FunctionCall: &FunctionCall{ID: st.id, Name: st.name, Args: args}})
	}
	if len(parts) > 0 {
		if err := s.frame(parts, "", nil); err != nil {
			return err
		}
		s.sent = true
	}

	// Ensure at least one frame is sent so clients receive finishReason.
	if !s.sent {
		if err := s.frame([]Part{{Text: ""}}, s.finish, nil); err != nil {
			return err
		}
	} else if err := s.frame(nil, s.finish, s.usage); err != nil {
		return err
	}
	s.done = true
	return nil
}

// FinishWithError emits a Google RPC error instead of model-generated text.
func (s *StreamWriter) FinishWithError(msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	raw, err := json.Marshal(RenderError(502, "UNAVAILABLE", msg))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", raw); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.done = true
	return nil
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
