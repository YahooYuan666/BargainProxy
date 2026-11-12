package gateway

// BP-314: the client-facing wording for a model capacity wall must name the
// real problem and the real remedy.
//
// Field 2026-11-12: ZCode surfaced "upstream rate limited: antigravity:
// {429 RESOURCE_EXHAUSTED}" for gemini-3.8-flash-high and the user reasonably
// asked "how can there possibly be no quota left?" — every account showed
// five_hour at 100%. Two separate lies were stacked here:
//
//  1. the 503 capacity wall was laundered into a 429 quota wall by the host
//     fallback (fixed in the driver), and
//  2. the busy-class wording blamed a "full queue", which suggests retrying,
//     when the only useful action is to switch models.
//
// The plain queue-busy path (CodeBuddy 6021) must keep its old wording — a deep
// queue IS worth waiting out.

import (
	"strings"
	"testing"

	"bargainproxy/internal/uperr"
)

func TestCapacityWallWordingSaysSwitchModels(t *testing.T) {
	e := uperr.New(uperr.ClassUpstreamBusy, 503, "antigravity: No capacity available for model gemini-3.8-flash-high on the server")
	e.Model = "gemini-3.8-flash-high"

	status, class, msg := DescribeError(e)
	if status != 429 {
		t.Fatalf("status = %d, want 429 (clients retry on 429, abort on 503)", status)
	}
	if class != uperr.ClassUpstreamBusy {
		t.Fatalf("class = %q, want upstream_busy", class)
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "queue is full") || strings.Contains(lower, "queue full") {
		t.Fatalf("message still blames a queue: %q", msg)
	}
	if !strings.Contains(msg, "switch models") {
		t.Fatalf("message = %q, want it to advise switching models — the same accounts serve other models", msg)
	}
	if !strings.Contains(msg, "gemini-3.8-flash-high") {
		t.Fatalf("message = %q, want it to name the model that has no capacity", msg)
	}
	if !strings.Contains(lower, "not a quota problem") {
		t.Fatalf("message = %q, want it to rule out a quota problem explicitly — that is the reading that was wrong", msg)
	}
}

// Control: the ordinary queue-busy verdict (no Model, or a non-503 status)
// keeps the established wording.
func TestQueueBusyWordingUnchanged(t *testing.T) {
	e := uperr.New(uperr.ClassUpstreamBusy, 429, "workbuddy: 排队中，请稍后")
	_, _, msg := DescribeError(e)
	if strings.Contains(msg, "switch models") {
		t.Fatalf("queue-busy message = %q; a deep queue is worth waiting out, not switching", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "queue") {
		t.Fatalf("queue-busy message = %q, want the queue wording", msg)
	}
}
