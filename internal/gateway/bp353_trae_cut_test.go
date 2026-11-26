// BP-353 (magpie-community/plugins #1366, 0.2.48~0.2.54): the trae SOLO
// upstream cuts a segment and reports finish_reason:"stop" — a truncation
// disguised as a clean end. Two shapes were unreachable before:
//
//   - the ENTERPRISE path cuts at exactly 2000 tokens, and those models are not
//     in traeCutCaps (which holds only consumer-path measurements), so every
//     such turn went out truncated with a WARN at most;
//   - a segment that spent the whole cut on REASONING wrote no answer bytes, so
//     every prose-anchored rule refused it and starved.go's shape B needs a
//     client-declared budget — a trae request without max_tokens was rescued by
//     neither.
package gateway

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func traeReq(maxTokens int, thinking bool) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:     "deepseek-v4.1-flash",
		MaxTokens: maxTokens,
		Thinking:  &ir.ThinkingConfig{Enabled: thinking},
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: txtPart("write the essay")}},
	}
}

// ledgerWith builds a ledger from the shapes an upstream can produce.
func ledgerWith(text, thinking string, out int, sawTool bool) *streamLedger {
	l := &streamLedger{sawTool: sawTool}
	l.text.WriteString(text)
	l.thinking.WriteString(thinking)
	l.usage.OutputTokens = out
	l.usageSet = true
	return l
}

func bp353StopResp(r ir.StopReason) *ir.ChatResponse {
	return &ir.ChatResponse{StopReason: r, Message: ir.Message{Role: ir.RoleAssistant}}
}

// The headline: a clean stop whose output landed exactly on the enterprise cut
// cap continues, even though the model is not in the measured table.
func TestBP353Enterprise2000CutContinues(t *testing.T) {
	led := ledgerWith(strings.Repeat("a", 4000), "", 2000, false)
	cont, why, ok := traeCutContinuationPlan("some-enterprise-model", traeReq(32000, false), led, bp353StopResp(ir.StopEnd))
	if !ok {
		t.Fatal("a clean stop at exactly 2000 was left truncated — that is the enterprise cut")
	}
	if cont == nil {
		t.Fatal("no continuation request")
	}
	if !strings.Contains(why, "2000") {
		t.Fatalf("why = %q — the log must name the number", why)
	}
	if cont.Thinking == nil || cont.Thinking.Enabled {
		t.Fatalf("thinking = %+v, want disabled (freeing the budget is the point)", cont.Thinking)
	}
	// The continuation must carry a budget at least the cap, or it is re-cut.
	if cont.MaxTokens < traeEnterpriseCutCap {
		t.Fatalf("continuation max_tokens = %d, want >= %d", cont.MaxTokens, traeEnterpriseCutCap)
	}
}

// A clean stop at a NON-cut number stays truncated: that ending is real.
func TestBP353OrdinaryStopNotContinued(t *testing.T) {
	led := ledgerWith(strings.Repeat("a", 4000), "", 2500, false)
	if _, _, ok := traeCutContinuationPlan("some-enterprise-model", traeReq(32000, false), led, bp353StopResp(ir.StopEnd)); ok {
		t.Fatal("a 2500-token stop is not a cut — continuing rewrites the client's own semantics")
	}
}

// The registered consumer cap still works (no regression on BP-317).
func TestBP353RegisteredCapStillContinues(t *testing.T) {
	led := ledgerWith(strings.Repeat("a", 4000), "", 32000, false)
	_, why, ok := traeCutContinuationPlan("deepseek-v4.1-flash", traeReq(32000, false), led, bp353StopResp(ir.StopEnd))
	if !ok {
		t.Fatal("the registered 32000 cap stopped continuing")
	}
	if !strings.Contains(why, "32000") {
		t.Fatalf("why = %q", why)
	}
}

// The reasoning-starved shape: zero answer bytes, 2000 spent on reasoning.
func TestBP353ThinkingStarvedContinues(t *testing.T) {
	led := ledgerWith("", strings.Repeat("think", 500), 2000, false)
	cont, why, ok := traeCutContinuationPlan("deepseek-v4.1-flash", traeReq(0, true), led, bp353StopResp(ir.StopEnd))
	if !ok {
		t.Fatal("a segment that spent 2000 tokens on reasoning and wrote nothing was left empty")
	}
	if cont == nil || cont.Thinking == nil || cont.Thinking.Enabled {
		t.Fatalf("continuation must disable thinking: %+v", cont)
	}
	if cont.RequestedEffort != "" {
		t.Fatalf("effort gear %q survived with thinking off (a gear asks FOR thinking)", cont.RequestedEffort)
	}
	if cont.MaxTokens < traeThinkingStarvedCap {
		t.Fatalf("continuation max_tokens = %d, want >= %d", cont.MaxTokens, traeThinkingStarvedCap)
	}
	if !strings.Contains(why, "reasoning") {
		t.Fatalf("why = %q", why)
	}
}

// Zero bytes AND no reasoning is the BP-335 empty reply — the cascade owns it,
// not a continuation (a second empty answer would just cost another call).
func TestBP353NothingAtAllIsNotContinued(t *testing.T) {
	led := ledgerWith("", "", 0, false)
	if _, _, ok := traeCutContinuationPlan("m", traeReq(0, true), led, bp353StopResp(ir.StopEnd)); ok {
		t.Fatal("an all-empty turn belongs to the empty-reply net, not to a continuation")
	}
}

// Thinking already off: freeing the budget cannot change anything.
func TestBP353ThinkingStarvedSkipsWhenThinkingOff(t *testing.T) {
	led := ledgerWith("", strings.Repeat("think", 500), 2000, false)
	if _, _, ok := traeCutContinuationPlan("m", traeReq(0, false), led, bp353StopResp(ir.StopEnd)); ok {
		t.Fatal("a thinking-off request with no answer cannot be rescued by turning thinking off")
	}
}

// A tool cut is the BP-139 redo shape, never a prose continuation.
func TestBP353ToolCutNotContinued(t *testing.T) {
	led := ledgerWith("", strings.Repeat("t", 100), 2000, true)
	if _, _, ok := traeCutContinuationPlan("m", traeReq(0, true), led, bp353StopResp(ir.StopEnd)); ok {
		t.Fatal("a cut inside a tool call is not this path's business")
	}
	led2 := ledgerWith(strings.Repeat("a", 100), "", 2000, true)
	if _, _, ok := traeCutContinuationPlan("m", traeReq(32000, false), led2, bp353StopResp(ir.StopEnd)); ok {
		t.Fatal("a prose cut that also saw a tool call must not continue")
	}
}

// An honest finish=length on trae has no measured mechanism — leave it.
func TestBP353LengthNotContinued(t *testing.T) {
	led := ledgerWith(strings.Repeat("a", 4000), "", 2000, false)
	if _, _, ok := traeCutContinuationPlan("m", traeReq(32000, false), led, bp353StopResp(ir.StopLength)); ok {
		t.Fatal("trae finish=length has no measured mechanism; continuing invents semantics")
	}
}

// The cumulative spending gate holds on the thinking-starved path too.
func TestBP353ThinkingStarvedRespectsCap(t *testing.T) {
	// floor×4 with a high effort gear is large; use the default 32768 case by
	// having no gear and a huge reported output.
	led := ledgerWith("", strings.Repeat("t", 100), defaultContinueTotalCap, false)
	req := traeReq(0, true)
	req.RequestedEffort = "" // no gear → defaultContinueTotalCap
	if _, _, ok := traeCutContinuationPlan("m", req, led, bp353StopResp(ir.StopEnd)); ok {
		t.Fatal("the cumulative cap was not enforced on the thinking-starved path")
	}
}
