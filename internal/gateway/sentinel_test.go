package gateway

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

func collect() (*[]ir.StreamEvent, driver_Emit) {
	var events []ir.StreamEvent
	return &events, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}
}

// driver_Emit mirrors driver.Emit for the test without importing driver
// (avoids an import cycle in test scope only if needed); alias to be simple.
type driver_Emit = func(ir.StreamEvent) error

func TestSentinelRejectsExactMatch(t *testing.T) {
	var events []ir.StreamEvent
	emit := func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}
	guard := sentinelGuard(emit, []string{"DANGEROUS_ACTIVITY_BLOCKED"})
	if err := guard(ir.StreamEvent{Kind: ir.EvText, Delta: "DANGEROUS_ACTIVITY_BLOCKED"}); err != nil {
		t.Fatalf("buffering should not error: %v", err)
	}
	err := guard(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSentinel {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("nothing must be emitted on sentinel rejection, got %d events", len(events))
	}
}

func TestSentinelPassesNormalReply(t *testing.T) {
	var events []ir.StreamEvent
	emit := func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}
	guard := sentinelGuard(emit, []string{"DANGEROUS_ACTIVITY_BLOCKED"})
	_ = guard(ir.StreamEvent{Kind: ir.EvText, Delta: "Hel"})
	_ = guard(ir.StreamEvent{Kind: ir.EvText, Delta: "lo"})
	if err := guard(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
		t.Fatalf("normal reply must not error: %v", err)
	}
	var text strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("text = %q", text.String())
	}
	// The first emitted event must be one flushed text event.
	if len(events) != 2 || events[0].Kind != ir.EvText || events[0].Delta != "Hello" || events[1].Kind != ir.EvDone {
		t.Fatalf("events = %+v", events)
	}
}

func TestSentinelFlushesOnNonText(t *testing.T) {
	var events []ir.StreamEvent
	emit := func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}
	guard := sentinelGuard(emit, []string{"SENTINEL"})
	_ = guard(ir.StreamEvent{Kind: ir.EvText, Delta: "partial"})
	_ = guard(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "t", Name: "f"}})
	if len(events) != 2 || events[0].Kind != ir.EvText || events[1].Kind != ir.EvToolStart {
		t.Fatalf("events = %+v", events)
	}
}

func TestSentinelSkipsLongReplies(t *testing.T) {
	var events []ir.StreamEvent
	emit := func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}
	guard := sentinelGuard(emit, []string{"SENTINEL"})
	long := strings.Repeat("x", sentinelMaxLen+1)
	if err := guard(ir.StreamEvent{Kind: ir.EvText, Delta: long}); err != nil {
		t.Fatalf("long reply should stream: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("long reply should flush immediately, events=%d", len(events))
	}
}
