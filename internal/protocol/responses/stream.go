package responses

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/ir"
)

// toolItemState is the per-tool-call state the streamed wire needs. The
// Responses protocol identifies a function_call by item id in some events and
// by output_index in others; one state keeps the id, call_id, name and the
// accumulated arguments consistent across added/delta/done (BP-69).
type toolItemState struct {
	itemID    string
	callID    string
	name      string
	args      strings.Builder
	announced bool
}

// StreamWriter drives the Responses SSE state machine from IR events.
type StreamWriter struct {
	w       io.Writer
	flusher interface{ Flush() }
	id      string
	model   string
	created int64

	mu        sync.Mutex
	created0  bool
	completed bool
	seq       int
	itemOpen  bool
	partOpen  bool
	textBuf   string
	usage     *Usage
	toolItems map[int]*toolItemState
}

// NewStreamWriter builds a stream writer.
func NewStreamWriter(w io.Writer, flusher interface{ Flush() }, id, model string) *StreamWriter {
	return &StreamWriter{w: w, flusher: flusher, id: id, model: model, created: time.Now().Unix(), toolItems: map[int]*toolItemState{}}
}

func toolItemID(index int, callID string) string {
	if callID != "" {
		return "fc_" + callID
	}
	return "fc_" + strconv.Itoa(index)
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
	case ir.EvAttemptReset:
		// BP-123: a failed gateway attempt is being retried on this same
		// client stream. This writer streams tool deltas immediately, so a
		// retry after tool fragments never happens (the gateway refuses to
		// cascade once content was delivered) — but the usage-only drop path
		// can still land here, and stale tool items would then collide with
		// the next attempt's indexes. Cheap to clear.
		s.toolItems = map[int]*toolItemState{}
		return nil
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
		st := s.toolItems[ev.ToolIndex]
		if st == nil {
			st = &toolItemState{}
			s.toolItems[ev.ToolIndex] = st
		}
		if ev.ToolCall != nil {
			if ev.ToolCall.ID != "" {
				st.callID = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				st.name = ev.ToolCall.Name
			}
		}
		if st.itemID == "" {
			st.itemID = toolItemID(ev.ToolIndex, st.callID)
		}
		return s.announceToolItem(ev.ToolIndex, st)
	case ir.EvToolArgs:
		st := s.toolItems[ev.ToolIndex]
		if st == nil {
			// Arguments before their start event: never drop the text — the
			// done event still carries the full call. The added event has no
			// name yet; drivers emit EvToolStart first in practice.
			st = &toolItemState{itemID: toolItemID(ev.ToolIndex, "")}
			s.toolItems[ev.ToolIndex] = st
		}
		if err := s.announceToolItem(ev.ToolIndex, st); err != nil {
			return err
		}
		st.args.WriteString(ev.Delta)
		return s.event("response.function_call_arguments.delta", map[string]any{
			"item_id": st.itemID, "output_index": 1 + ev.ToolIndex, "delta": ev.Delta,
		})
	case ir.EvUsage:
		s.usage = usageFromIR(*ev.Usage)
		return nil
	case ir.EvDone:
		// BP-181: a length-capped turn ends incomplete, not completed —
		// Codex-class clients read the status to decide whether to continue.
		return s.finish(ev.Stop == ir.StopLength)
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

// announceToolItem emits the output_item.added event once per tool call.
func (s *StreamWriter) announceToolItem(toolIndex int, st *toolItemState) error {
	if st.announced {
		return nil
	}
	st.announced = true
	item := map[string]any{
		"type": "function_call", "id": st.itemID, "call_id": st.callID,
		"name": st.name, "arguments": "", "status": "in_progress",
	}
	return s.event("response.output_item.added", map[string]any{"output_index": 1 + toolIndex, "item": item})
}

func (s *StreamWriter) finish(incomplete bool) error {
	if err := s.ensureCreated(); err != nil {
		return err
	}
	// Close open tool items in output order: the done events carry the full
	// call (name/call_id/arguments). Clients such as ZCode assemble their
	// final tool-call from exactly these fields (BP-69).
	idxs := make([]int, 0, len(s.toolItems))
	for idx := range s.toolItems {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, toolIdx := range idxs {
		st := s.toolItems[toolIdx]
		args := st.args.String()
		if args == "" {
			args = "{}"
		}
		callID := st.callID
		if callID == "" {
			callID = st.itemID
		}
		if err := s.event("response.function_call_arguments.done", map[string]any{
			"item_id": st.itemID, "output_index": 1 + toolIdx, "arguments": args,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "function_call", "id": st.itemID, "call_id": callID,
			"name": st.name, "arguments": args, "status": "completed",
		}
		if err := s.event("response.output_item.done", map[string]any{
			"output_index": 1 + toolIdx, "item": item,
		}); err != nil {
			return err
		}
	}
	if s.itemOpen {
		if err := s.closeMessageItem(); err != nil {
			return err
		}
	}
	status := "completed"
	event := "response.completed"
	if incomplete {
		// BP-181 (sub2api 9b540744): the terminal object carries the real
		// status plus the standard incomplete_details reason.
		status = "incomplete"
		event = "response.incomplete"
	}
	env := s.responseEnvelope(status)
	if incomplete {
		env["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if s.usage != nil {
		env["usage"] = s.usage
	}
	if err := s.event(event, map[string]any{"response": env}); err != nil {
		return err
	}
	s.completed = true
	return nil
}

// FinishWithError closes the stream with a failed status after a mid-stream
// upstream failure.
func (s *StreamWriter) FinishWithError(msg string) error {
	return s.FinishWithErrorCode("stream_error", msg)
}

// FinishWithErrorCode is FinishWithError with an explicit error code.
//
// BP-244 (magpie 2cca1117, #4425): the code is what a client branches on, and
// a context overflow needs `context_length_exceeded` — the one code Codex
// compacts on. It used to always be `stream_error`, so an oversized prompt
// looked like a transient stream fault and Codex reconnected with the identical
// body five times instead of compacting.
func (s *StreamWriter) FinishWithErrorCode(code, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed {
		return nil
	}
	_ = s.openMessageItemLocked()
	env := s.responseEnvelope("failed")
	env["error"] = map[string]any{"code": code, "message": msg}
	if err := s.event("response.failed", map[string]any{"response": env}); err != nil {
		return err
	}
	s.completed = true
	return nil
}

func (s *StreamWriter) openMessageItemLocked() error {
	return s.openMessageItem()
}
