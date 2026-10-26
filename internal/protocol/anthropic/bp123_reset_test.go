package anthropic

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// render runs one Send sequence against a capturing writer and returns the raw
// SSE payload, so the test can assert on the tool_use block the client sees.
func render(t *testing.T, sends ...ir.StreamEvent) string {
	t.Helper()
	var out strings.Builder
	sw := NewStreamWriter(&out, nil, "msg_x", "m", 1)
	for _, ev := range sends {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("Send %s: %v", ev.Kind, err)
		}
	}
	return out.String()
}

// BP-123: tool blocks are buffered until the turn ends (BP-70). When the
// gateway retries a dead attempt on the same client stream, the dead attempt's
// buffered fragments must be discarded — or the retry's arguments concatenate
// onto them and the client receives invalid JSON.
//
// Field shape 2026-10-26 22:19: workbuddy streamed the start of a tool call,
// then the socket died mid-arguments; the retry re-ran the whole turn.
func TestResetDiscardsBufferedToolFragments(t *testing.T) {
	out := render(t,
		// Dead attempt: tool call starts, arguments are truncated mid-JSON.
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_dead", Name: "Write"}},
		ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md","content":"partial`},
		// The retry announces itself and re-runs the turn.
		ir.StreamEvent{Kind: ir.EvAttemptReset},
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_live", Name: "Write"}},
		ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md","content":"full"}`},
		ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse},
	)
	if !strings.Contains(out, "call_live") {
		t.Fatalf("retry's tool block missing from the stream: %s", out)
	}
	// Both attempts wrote path a.md; after the reset only the retry's copy
	// may remain. (The raw JSON is escaped on the wire, so count on a stable
	// token instead of matching the escaped quote soup.)
	if n := strings.Count(out, `a.md`); n != 1 {
		t.Fatalf("dead attempt's fragment leaked into the retried stream (a.md appears %d times, want 1): %s", n, out)
	}
	if n := strings.Count(out, `"type":"tool_use"`); n != 1 {
		t.Fatalf("expected exactly one tool block, got %d: %s", n, out)
	}
}

// TestWithoutResetFragmentsConcatenate documents the hazard the reset removes:
// without it the two attempts' argument fragments land in one block.
func TestWithoutResetFragmentsConcatenate(t *testing.T) {
	// NO reset — the pre-BP-123 behaviour: the retry appends onto the fragment
	// and the client receives one block with BOTH attempts' JSON glued.
	out := render(t,
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_dead", Name: "Write"}},
		ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md","content":"partial`},
		ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_live", Name: "Write"}},
		ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md","content":"full"}`},
		ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse},
	)
	// Without the reset both fragments land in ONE block: a.md appears twice.
	if n := strings.Count(out, `a.md`); n != 2 {
		t.Fatal("precondition lost: without the reset the fragments DO concatenate (this is what the reset prevents)")
	}
}
