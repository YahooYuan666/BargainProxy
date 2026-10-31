package qoder

import "testing"

// BP-222: the accumulator must remember the raw finish_reason "error" even
// though finalFinishReason() would override it (tool_calls / stop), so the
// Stream loop can fail the attempt instead of building a success-shaped
// response (magpie 86fef947 #2471).
func TestBP222AccumulatorRemembersFailedFinish(t *testing.T) {
	acc := newStreamAccumulator("r", 1, "m", false, func(string) {})

	acc.accept(&bridgeDelta{FinishReason: "error"})
	if !acc.failedFinish {
		t.Fatal("finish_reason \"error\" must set failedFinish")
	}
	if acc.finalFinishReason() != "error" {
		t.Fatalf("finalFinishReason = %q, want the raw error word", acc.finalFinishReason())
	}

	// Other words never trip the flag — a tool-call stream stays normal.
	acc2 := newStreamAccumulator("r", 1, "m", false, func(string) {})
	acc2.accept(&bridgeDelta{FinishReason: "tool_calls"})
	if acc2.failedFinish {
		t.Fatal("tool_calls must not set failedFinish")
	}
	if acc2.finalFinishReason() != "tool_calls" {
		t.Fatalf("finalFinishReason = %q, want tool_calls", acc2.finalFinishReason())
	}

	acc3 := newStreamAccumulator("r", 1, "m", false, func(string) {})
	acc3.accept(&bridgeDelta{FinishReason: "stop"})
	if acc3.failedFinish {
		t.Fatal("stop must not set failedFinish")
	}
}
