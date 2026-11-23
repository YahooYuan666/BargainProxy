// BP-335: empty-reply detection. A turn that terminates CLEANLY having
// produced nothing (no text, no reasoning, no tool call) used to reach the
// success branch: ApplyResult(nil), sticky kept, 200 with an empty body, and
// the client reported "Stream ended without finish_reason" and never retried.
//
// Two upstreams converged on the same defect the same week — magpie d847b796
// (Copilot gemini-4-hydrogen answering role chunk + usage + [DONE] with no
// content) and CLIProxyAPI 892fc486 (readStreamBootstrap labelling "zero
// payload before EOF" as a retryable empty_stream).
package gateway

import (
	"testing"

	"bargainproxy/internal/ir"
)

func emptyReq(maxTokens int) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:     "gemini-4-hydrogen",
		MaxTokens: maxTokens,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: txtPart("hi")}},
	}
}

// The headline shape: a clean termination with nothing in the ledger.
func TestEmptyReplyDetected(t *testing.T) {
	if !emptyReplyOf(emptyReq(100), &streamLedger{}) {
		t.Fatal("a clean turn with zero text/thinking/tools must be an empty reply")
	}
}

// A liveness probe (max_tokens=1) is SUPPOSED to answer nothing; retrying it
// would burn a second request to learn the same thing.
func TestEmptyReplySkipsProbes(t *testing.T) {
	for _, mt := range []int{0, 1, 8, minStarvedBudget - 1} {
		if emptyReplyOf(emptyReq(mt), &streamLedger{}) {
			t.Fatalf("max_tokens=%d must not be treated as an empty reply", mt)
		}
	}
}

// Anything the client can act on disqualifies the shape — including a tool
// call whose arguments never streamed as text.
func TestEmptyReplyRequiresNothingAtAll(t *testing.T) {
	led := &streamLedger{}
	led.text.WriteString("hi")
	if emptyReplyOf(emptyReq(100), led) {
		t.Fatal("text must disqualify the empty-reply shape")
	}

	led = &streamLedger{}
	led.thinking.WriteString("thinking...")
	if emptyReplyOf(emptyReq(100), led) {
		t.Fatal("reasoning must disqualify the empty-reply shape (that is starved shape B's job)")
	}

	led = &streamLedger{}
	led.sawTool = true
	if emptyReplyOf(emptyReq(100), led) {
		t.Fatal("a tool call must disqualify the empty-reply shape")
	}
}

// The starved rescue must NOT claim this shape: it needs finish=length or
// streamed reasoning, so an all-empty turn falls through it. This is the
// regression that made the empty reply reach the success branch.
func TestEmptyReplyIsNotStarvedShape(t *testing.T) {
	req := emptyReq(100)
	req.Thinking = &ir.ThinkingConfig{Enabled: true}
	resp := &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: txtPart("")},
		StopReason: ir.StopEnd,
	}
	if _, ok := starvedRetryRequest(req, resp, &streamLedger{}); ok {
		t.Fatal("an all-empty turn must not match the starved rescue (no length, no reasoning)")
	}
	if !emptyReplyOf(req, &streamLedger{}) {
		t.Fatal("...but it must match the empty-reply guard, or nothing catches it")
	}
}

func TestEmptyReplyNilSafety(t *testing.T) {
	if emptyReplyOf(nil, &streamLedger{}) || emptyReplyOf(emptyReq(100), nil) {
		t.Fatal("nil request or ledger must not report an empty reply")
	}
}
