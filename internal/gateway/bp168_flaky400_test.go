package gateway

import (
	"context"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// flaky400Driver answers a lying 400 first (field evidence 2026-10-28:
// Google's antigravity rejected the 54th request of a healthy session with a
// generic INVALID_ARGUMENT), then succeeds. goodAfter controls how many
// initial 400s come before the success.
type flaky400Driver struct {
	calls     int
	goodAfter int
	emitFirst bool // emit text BEFORE failing, to pin the zero-output guard
}

func (d *flaky400Driver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *flaky400Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *flaky400Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	if d.emitFirst {
		// Well past the sentinel hold-back (128 bytes): only delivered text
		// may suppress the resend.
		_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: strings.Repeat("partial ", 30)})
	}
	if d.calls <= d.goodAfter {
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"antigravity: Request contains an invalid argument.")
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func bp168Request() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "m", Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// BP-168: one lying 400 with nothing emitted must be resent once to the SAME
// account, and the turn succeeds.
func TestBP168Flaky400ResentOnce(t *testing.T) {
	drv := &flaky400Driver{goodAfter: 1}
	g := newGatewayWithTwoBindings(t, drv)

	var text strings.Builder
	if _, err := g.Execute(context.Background(), bp168Request(), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("flaky 400 not survived: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if drv.calls != 2 {
		t.Fatalf("calls = %d, want 2 (original + one resend)", drv.calls)
	}
}

// A deterministic 400 gets exactly ONE resend and then reaches the client.
// bad_request never rotates WITHIN a channel but DOES cascade to the next
// binding (BP-146: one upstream's strictness is not a global truth), so the
// call count is wb(original+resend) + trae(original) = 3 — and the per-request
// pin is what keeps it at 3 instead of 4.
func TestBP168Deterministic400ResendsOnlyOnce(t *testing.T) {
	drv := &flaky400Driver{goodAfter: 999}
	g := newGatewayWithTwoBindings(t, drv)

	_, err := g.Execute(context.Background(), bp168Request(), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("a forever-400 must fail the request")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %v, want bad_request", ue)
	}
	if drv.calls != 3 {
		t.Fatalf("calls = %d, want 3 (wb original+resend, trae original — the pin stops a second resend)", drv.calls)
	}
}

// A 400 AFTER delivered output is never resent — the zero-output guard is
// what makes the resend safe.
func TestBP168NoResendAfterOutput(t *testing.T) {
	drv := &flaky400Driver{goodAfter: 999, emitFirst: true}
	g := newGatewayWithTwoBindings(t, drv)

	_, err := g.Execute(context.Background(), bp168Request(), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("must fail")
	}
	if drv.calls != 1 {
		t.Fatalf("calls = %d, want 1 (output already on the wire — no resend, no cascade)", drv.calls)
	}
}
