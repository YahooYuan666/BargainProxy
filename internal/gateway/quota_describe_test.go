// The quota-exhausted message used to read as if every pool were frozen; it
// is the LAST binding's error after earlier bindings failed. It must name the
// channel so the operator knows which pool is out (2026-10-18 user report:
// WorkBuddy looked frozen while only Qoder's quota was exhausted).
package gateway

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

func TestDescribeErrorQuotaNamesChannel(t *testing.T) {
	ue := uperr.New(uperr.ClassQuotaExhausted, 429, "no available account on channel qoder")
	ue.Channel = "qoder"
	ue.ResetAt = time.Date(2026, 10, 18, 12, 12, 20, 0, time.UTC)

	status, class, msg := DescribeError(ue)
	if status != 429 || class != uperr.ClassQuotaExhausted {
		t.Fatalf("status/class = %d/%s, want 429/%s", status, class, uperr.ClassQuotaExhausted)
	}
	if !strings.Contains(msg, "on qoder") {
		t.Fatalf("message must name the channel: %q", msg)
	}
	if !strings.Contains(msg, "earliest reset at") {
		t.Fatalf("message must keep the reset hint: %q", msg)
	}
}
