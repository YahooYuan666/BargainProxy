package responses

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"bargainproxy/internal/ir"
)

// StreamWriter drives the Responses SSE state machine from IR events.
type StreamWriter struct {
	w       io.Writer
	flusher interface{ Flush() }
	id      string
	model   string
	created int64

	mu           sync.Mutex
	created0     bool
	completed    bool
	seq          int
	itemOpen     bool
	partOpen     bool
	textBuf      string
	usage        *Usage
	toolItemOpen map[int]bool
}

// NewStreamWriter builds a stream writer.
func NewStreamWriter(w io.Writer, flusher interface{ Flush() }, id, model string) *StreamWriter {
	return &StreamWriter{w: w, flusher: flusher, id: id, model: model, created: time.Now().Unix(), toolItemOpen: map[int]bool{}}
}

func (s *StreamWriter) event(name string, payload map[string]any) error {
	payload["type"] = name
	payload["sequence_number"] = s.seq
	s.seq++
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

func (s *StreamWriter) responseEnvelope(status string) map[string]any {
	return map[string]any{
		"id":         s.id,
		"object":     "response",
		"created_at": s.created,
		"status":     status,
		"model":      s.model,
		"output":     []any{},
	}
}

func (s *StreamWriter) ensureCreated() error {
	if s.created0 {
		return nil
	}
	s.created0 = true
	env := s.responseEnvelope("in_progress")
	env["output"] = []any{}
	if err := s.event("response.created", map[string]any{"response": env}); err != nil {
		return err
	}
	return s.event("response.in_progress", map[string]any{"response": s.responseEnvelope("in_progress")})
}

// openMessageItem opens the assistant message output item and its
// output_text content part. Idempotent.
func (s *StreamWriter) openMessageItem() error {
	if s.itemOpen {
		return nil
	}
	if err := s.ensureCreated(); err != nil {
		return err
	}
	item := map[string]any{
		"type": "message", "id": "msg_" + s.id, "status": "in_progress",
		"role": "assistant", "content": []any{},
	}
	if err := s.event("response.output_item.added", map[string]any{"output_index": 0, "item": item}); err != nil {
		return err
	}
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	if err := s.event("response.content_part.added", map[string]any{"item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "part": part}); err != nil {
		return err
	}
	s.itemOpen = true
	s.partOpen = true
	return nil
}

// Send routes one IR event into the Responses stream.
func (s *StreamWriter) Send(ev ir.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed {
		return nil
	}
	switch ev.Kind {
	case ir.EvText:
		if err := s.openMessageItem(); err != nil {
			return err
		}
		s.textBuf += ev.Delta
		return s.event("response.output_text.delta", map[string]any{
			"item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "delta": ev.Delta,
		})
	case ir.EvThinking:
		// Reasoning summaries are optional in the Responses protocol; we
		// deliberately drop them to keep the event surface small.
		return nil
	case ir.EvToolStart:
		if err := s.ensureCreated(); err != nil {
			return err
		}
		if s.itemOpen {
			if err := s.closeMessageItem(); err != nil {
				return err
			}
		}
		idx := 1 + ev.ToolIndex
		item := map[string]any{
			"type": "function_call", "id": "fc_" + ev.ToolCall.ID, "call_id": ev.ToolCall.ID,
			"name": ev.ToolCall.Name, "arguments": "", "status": "in_progress",
		}
		s.toolItemOpen[ev.ToolIndex] = true
		return s.event("response.output_item.added", map[string]any{"output_index": idx, "item": item})
	case ir.EvToolArgs:
		if !s.toolItemOpen[ev.ToolIndex] {
			return nil
		}
		idx := 1 + ev.ToolIndex
		return s.event("response.function_call_arguments.delta", map[string]any{
			"item_id": fmt.Sprintf("fc_%d", ev.ToolIndex), "output_index": idx, "delta": ev.Delta,
		})
	case ir.EvUsage:
		s.usage = &Usage{
			InputTokens:  ev.Usage.InputTokens,
			OutputTokens: ev.Usage.OutputTokens,
			TotalTokens:  ev.Usage.InputTokens + ev.Usage.OutputTokens,
		}
		return nil
	case ir.EvDone:
		return s.finish()
	}
	return nil
}

func (s *StreamWriter) closeMessageItem() error {
	if !s.itemOpen {
		return nil
	}
	if err := s.event("response.output_text.done", map[string]any{
		"item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "text": s.textBuf,
	}); err != nil {
		return err
	}
	part := map[string]any{"type": "output_text", "text": s.textBuf, "annotations": []any{}}
	if err := s.event("response.content_part.done", map[string]any{
		"item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "part": part,
	}); err != nil {
		return err
	}
	item := map[string]any{
		"type": "message", "id": "msg_" + s.id, "status": "completed",
		"role": "assistant", "content": []any{part},
	}
	if err := s.event("response.output_item.done", map[string]any{"output_index": 0, "item": item}); err != nil {
		return err
	}
	s.itemOpen = false
	s.partOpen = false
	return nil
}

func (s *StreamWriter) finish() error {
	if err := s.ensureCreated(); err != nil {
		return err
	}
	// Close any open tool items.
	for toolIdx := range s.toolItemOpen {
		idx := 1 + toolIdx
		if err := s.event("response.function_call_arguments.done", map[string]any{
			"item_id": fmt.Sprintf("fc_%d", toolIdx), "output_index": idx, "arguments": "{}",
		}); err != nil {
			return err
		}
		if err := s.event("response.output_item.done", map[string]any{
			"output_index": idx,
			"item":         map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", toolIdx), "status": "completed"},
		}); err != nil {
			return err
		}
	}
	if s.itemOpen {
		if err := s.closeMessageItem(); err != nil {
			return err
		}
	}
	env := s.responseEnvelope("completed")
	if s.usage != nil {
		env["usage"] = s.usage
	}
	if err := s.event("response.completed", map[string]any{"response": env}); err != nil {
		return err
	}
	s.completed = true
	return nil
}

// FinishWithError closes the stream with a failed status after a mid-stream
// upstream failure.
func (s *StreamWriter) FinishWithError(msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed {
		return nil
	}
	_ = s.openMessageItemLocked()
	env := s.responseEnvelope("failed")
	env["error"] = map[string]any{"code": "stream_error", "message": msg}
	if err := s.event("response.failed", map[string]any{"response": env}); err != nil {
		return err
	}
	s.completed = true
	return nil
}

func (s *StreamWriter) openMessageItemLocked() error {
	return s.openMessageItem()
}
