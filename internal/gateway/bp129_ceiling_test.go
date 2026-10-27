package gateway

import (
	"context"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// ceilingDriver refuses any request whose max_tokens exceeds 32768 with the
// exact wording Qoder uses, and answers once the value is legal.
type ceilingDriver struct {
	calls  int
	gotMax []int
}

func (d *ceilingDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *ceilingDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *ceilingDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	d.gotMax = append(d.gotMax, call.Request.MaxTokens)
	if call.Request.MaxTokens > 32768 {
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"qoder: Range of max_tokens should be [1, 32768]")
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func ceilingRequest(n int) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: n,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// BP-129: "Range of max_tokens should be [1, N]" is repaired in-flight (clamp
// + retry on the same account — a bad_request never started the turn), the
// ceiling is learned, and later requests are clamped UPFRONT so they never
// pay the 400 again.
func TestMaxTokensCeilingLearnedAndClamped(t *testing.T) {
	drv := &ceilingDriver{}
	g := newGatewayWithTwoBindings(t, drv)

	var text strings.Builder
	if _, err := g.Execute(context.Background(), ceilingRequest(100000), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("Execute: %v (an over-ceiling max_tokens must be repaired, not fail the turn)", err)
	}
	if text.String() != "ok" {
		t.Fatalf("client saw %q", text.String())
	}
	if len(drv.gotMax) < 2 || drv.gotMax[0] != 100000 || drv.gotMax[1] != 32768 {
		t.Fatalf("repair sequence = %v, want [100000 32768 …]", drv.gotMax)
	}
	if got := config.LearnedOutputCeiling(model.ChannelWorkBuddy, "m"); got != 32768 {
		t.Fatalf("learned ceiling = %d, want 32768", got)
	}

	// A later request with the same oversized value never sees the 400. The
	// FIRST Execute's opening attempt (100000) is the discovery probe and is
	// expected; everything AFTER it must be clamped.
	before := len(drv.gotMax)
	if _, err := g.Execute(context.Background(), ceilingRequest(100000), func(ir.StreamEvent) error {
		return nil
	}); err != nil {
		t.Fatalf("second Execute: %v (learned ceiling must clamp upfront)", err)
	}
	for i, m := range drv.gotMax[before:] {
		if m > 32768 {
			t.Fatalf("post-learning call %d went out with max_tokens=%d, all must be clamped", i, m)
		}
	}
}
