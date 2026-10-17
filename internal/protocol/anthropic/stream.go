package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"bargainproxy/internal/ir"
)

// StreamWriter converts IR stream events into an Anthropic Messages SSE
// stream. Event sequence per the protocol:
//
//	message_start -> (content_block_start -> content_block_delta* ->
//	content_block_stop)* -> message_delta -> message_stop
//
// Block indexing: thinking block (if any) opens first at index 0, text at
// the next index, then one block per tool call. Deltas are routed to the
// currently open block by kind.
type StreamWriter struct {
	w           io.Writer
	flusher     interface{ Flush() }
	id          string
	model       string
	mu          sync.Mutex
	started     bool
	closed      bool
	blockIdx    int
	openKind    string // "", "text", "thinking", "tool"
	openToolIdx int    // tool index when openKind == "tool"

	// usage is the upstream's authoritative accounting for this response,
	// kept whole so message_delta can emit the Anthropic split (UsageFromIR).
	usage *ir.Usage
	// initInput is the request-side prompt estimate announced in
	// message_start (the upstream has not reported yet) — see NewStreamWriter.
	initInput   int
	sawThinking bool
	textStarted bool
}

// NewStreamWriter builds a writer for one response. estInputTokens is the
// request-side prompt estimate (protocol/anthropic.EstimateTokens, the same
// number count_tokens exposes); it fills message_start's input_tokens as a
// placeholder — never a fabricated 0 for a non-empty prompt — and
// message_delta then overwrites it with the upstream's authoritative usage.
func NewStreamWriter(w io.Writer, flusher interface{ Flush() }, id, model string, estInputTokens int) *StreamWriter {
	return &StreamWriter{w: w, flusher: flusher, id: id, model: model, blockIdx: -1, initInput: estInputTokens}
}

func (s *StreamWriter) event(name string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

func (s *StreamWriter) messageStart() error {
	empty := make([]ContentBlock, 0)
	return s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.id,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       empty,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         Usage{InputTokens: s.initInput},
		},
	})
}

func (s *StreamWriter) closeBlock() error {
	if s.openKind == "" {
		return nil
	}
	idx := s.blockIdx
	s.openKind = ""
	return s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (s *StreamWriter) openText() error {
	if s.openKind == "text" {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.blockIdx++
	s.openKind = "text"
	s.textStarted = true
	return s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *StreamWriter) openThinking() error {
	if s.openKind == "thinking" {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.blockIdx++
	s.openKind = "thinking"
	s.sawThinking = true
	// `signature` stays present (empty until a real one exists): Anthropic
	// itself emits it on content_block_start and strict clients reject the
	// block otherwise — reference: sub2api apicompat types.go (2026-10-16).
	return s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
	})
}

func (s *StreamWriter) openTool(toolIndex int, tc *ir.ToolCall) error {
	if s.openKind == "tool" && s.openToolIdx == toolIndex {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.blockIdx++
	s.openKind = "tool"
	s.openToolIdx = toolIndex
	return s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIdx,
		"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}},
	})
}

// Send routes one IR event into the Anthropic stream.
func (s *StreamWriter) Send(ev ir.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if !s.started {
		if err := s.messageStart(); err != nil {
			return err
		}
		s.started = true
	}
	switch ev.Kind {
	case ir.EvText:
		if err := s.openText(); err != nil {
			return err
		}
		return s.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": ev.Delta},
		})
	case ir.EvThinking:
		if err := s.openThinking(); err != nil {
			return err
		}
		return s.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Delta},
		})
	case ir.EvToolStart:
		return s.openTool(ev.ToolIndex, ev.ToolCall)
	case ir.EvToolArgs:
		if err := s.openTool(ev.ToolIndex, &ir.ToolCall{ID: fmt.Sprintf("call_%d", ev.ToolIndex)}); err != nil {
			return err
		}
		return s.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Delta},
		})
	case ir.EvUsage:
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil
	case ir.EvDone:
		return s.finish(string(ev.Stop))
	}
	return nil
}

// finish closes open blocks and emits message_delta + message_stop.
func (s *StreamWriter) finish(stop string) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	reason := mapStop(ir.StopReason(stop))
	// The final usage is cumulative and authoritative: clients recompute the
	// prompt as input + cache_creation + cache_read, and the AI SDK overwrites
	// each field it finds here — so this event is what actually feeds a
	// client's context meter (ZCode's usage bar / auto-compact baseline).
	// Without upstream usage (cancelled mid-stream) keep the start estimate.
	usage := Usage{InputTokens: s.initInput}
	if s.usage != nil {
		usage = UsageFromIR(*s.usage)
	}
	if err := s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return err
	}
	if err := s.event("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// FinishWithError closes the stream after an upstream failure mid-stream:
// append a text note (clients render it) and stop.
func (s *StreamWriter) FinishWithError(msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if !s.started {
		if err := s.messageStart(); err != nil {
			return err
		}
		s.started = true
	}
	if err := s.openText(); err != nil {
		return err
	}
	if err := s.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.blockIdx,
		"delta": map[string]any{"type": "text_delta", "text": "\n[stream error: " + msg + "]"},
	}); err != nil {
		return err
	}
	return s.finish(string(ir.StopEnd))
}
