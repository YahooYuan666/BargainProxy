package anthropic

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-139: a continuation after a mid-call truncation emits EvToolRestart. The
// writer must retire the partial buffer (the client must never receive the
// truncated half-JSON) and map the continuation's fresh call — which restarts
// at driver index 0 — into a NEW block, not onto the dead fragment.
func TestToolRestartRetiresPartialAndShiftsIndexes(t *testing.T) {
	out := render(t,
		// Segment 1: prose delivered, tool call cut mid-arguments.
		ir.StreamEvent{Kind: ir.EvText, Delta: "让我查一下文件"},
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_cut", Name: "Read"}},
		ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md`},
		// Continuation splices in: gateway announces the tool restart.
		ir.StreamEvent{Kind: ir.EvToolRestart},
		// Segment 2: the model redoes the call (driver index 0 again).
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_redo", Name: "Read"}},
		ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md","content":"x"}`},
		ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse},
	)
	// The truncated fragment must never reach the wire: both the dead call
	// and the redo carry path a.md, so the count must be exactly 1 (redo).
	if n := strings.Count(out, `a.md`); n != 1 {
		t.Fatalf("a.md appears %d times, want 1 (fragment leaked or redo lost): %s", n, out)
	}
	if n := strings.Count(out, `"type":"tool_use"`); n != 1 {
		t.Fatalf("want exactly one (complete) tool block, got %d: %s", n, out)
	}
	if !strings.Contains(out, "call_redo") {
		t.Fatalf("the redo call missing: %s", out)
	}
	// The prose block must be intact (the restart must not have cleared text).
	if !strings.Contains(out, "让我查一下文件") {
		t.Fatalf("delivered text lost by the restart: %s", out)
	}
}

// Two redo calls (the continuation itself emits two) keep separate blocks.
func TestToolRestartAllowsMultipleRedoCalls(t *testing.T) {
	out := render(t,
		ir.StreamEvent{Kind: ir.EvText, Delta: "prose"},
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_cut", Name: "A"}},
		ir.StreamEvent{Kind: ir.EvToolRestart},
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "r1", Name: "A"}},
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 1, ToolCall: &ir.ToolCall{ID: "r2", Name: "B"}},
		ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse},
	)
	if n := strings.Count(out, `"type":"tool_use"`); n != 2 {
		t.Fatalf("want 2 tool blocks after restart, got %d: %s", n, out)
	}
}
