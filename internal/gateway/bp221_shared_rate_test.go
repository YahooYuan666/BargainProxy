package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// sharedRateDriver answers the shared free-lane 429 first (BP-221 field
// 2026-10-31: one refusal inside a burst of healthy requests), then succeeds.
type sharedRateDriver struct {
	calls     int
	goodAfter int
	emitFirst bool // emit text BEFORE failing, to pin the zero-output guard
}

func (d *sharedRateDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *sharedRateDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *sharedRateDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	if d.emitFirst {
		_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: strings.Repeat("partial ", 30)})
	}
	if d.calls <= d.goodAfter {
		ue := uperr.New(uperr.ClassSoftRate, 429, "opencodezen: free usage limit reached")
		ue.SharedQuota = true
		return nil, ue
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func bp221Request() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "m", Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// BP-221: one episodic shared-quota 429 with nothing emitted must be held and
// resent to the SAME account once, and the turn succeeds — the pool's short
// rest paces follow-ups, it does not fail this one.
func TestBP221SharedRateHeldAndResentOnce(t *testing.T) {
	oldHold := sharedRateHold
	sharedRateHold = 10 * time.Millisecond
	defer func() { sharedRateHold = oldHold }()
	drv := &sharedRateDriver{goodAfter: 1}
	g := newGatewayWithTwoBindings(t, drv)

	var text strings.Builder
	start := time.Now()
	if _, err := g.Execute(context.Background(), bp221Request(), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("episodic shared-rate 429 not survived: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if drv.calls != 2 {
		t.Fatalf("calls = %d, want 2 (original + one hold-resend)", drv.calls)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("resend fired without the hold window")
	}
}

// BP-221 night upgrade: the shared bucket refills on its own schedule, so the
// hold is a LOOP — three consecutive refusals are each held and resent, and
// the turn succeeds on the fourth upstream call.
func TestBP221PatientLoopRidesThreeRefusals(t *testing.T) {
	oldHold := sharedRateHold
	sharedRateHold = 10 * time.Millisecond
	defer func() { sharedRateHold = oldHold }()
	drv := &sharedRateDriver{goodAfter: 3}
	g := newGatewayWithTwoBindings(t, drv)

	var text strings.Builder
	if _, err := g.Execute(context.Background(), bp221Request(), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("three refusals not ridden out: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if drv.calls != 4 {
		t.Fatalf("calls = %d, want 4 (original + three holds)", drv.calls)
	}
}

// A refusal that never lifts is bounded: sharedRateMaxResends holds, then the
// honest error — not an endless hold.
func TestBP221PatientLoopIsBounded(t *testing.T) {
	oldHold := sharedRateHold
	sharedRateHold = 10 * time.Millisecond
	defer func() { sharedRateHold = oldHold }()
	drv := &sharedRateDriver{goodAfter: 1 << 30} // never succeeds
	g := newGatewayWithTwoBindings(t, drv)

	_, err := g.Execute(context.Background(), bp221Request(), func(ev ir.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("an exhausted lane must surface the error")
	}
	// One original + max resends on the FIRST binding; the second binding's
	// cascade adds one more attempt.
	if drv.calls > 1+sharedRateMaxResends+1 {
		t.Fatalf("calls = %d, want ≤ %d (bounded)", drv.calls, 1+sharedRateMaxResends+1)
	}
}

// With text already delivered, the hold must NOT fire: a mid-stream refusal
// rides the BP-107 continuation path, not a replay.
func TestBP221NoHoldAfterOutputReachedTheClient(t *testing.T) {
	drv := &sharedRateDriver{goodAfter: 1, emitFirst: true}
	g := newGatewayWithTwoBindings(t, drv)

	// The driver emitted before failing; the gateway must not resend. The
	// turn still fails (soft rate with no continuation trigger) — what the
	// test pins is the CALL COUNT: exactly one upstream attempt.
	_, _ = g.Execute(context.Background(), bp221Request(), func(ev ir.StreamEvent) error {
		return nil
	})
	if drv.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no hold-resend after delivered output)", drv.calls)
	}
}

func init() { sharedRateHold = 10 * time.Millisecond }

// A second shared-rate refusal inside the same request must NOT stack a
// second hold: the marker pins the branch to once per request, so a real
// exhaustion falls through to the pool honestly.
func TestBP221SecondRefusalDoesNotHoldAgain(t *testing.T) {
	drv := &sharedRateDriver{goodAfter: 2} // refuses twice, then succeeds
	g := newGatewayWithTwoBindings(t, drv)

	var text strings.Builder
	if _, err := g.Execute(context.Background(), bp221Request(), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("two refusals not survived via the cascade: %v", err)
	}
	if drv.calls != 3 {
		t.Fatalf("calls = %d, want 3 (hold once, then the second refusal cascades to the next binding)", drv.calls)
	}
}
