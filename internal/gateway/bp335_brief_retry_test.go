// BP-335: end-of-cascade brief retry. When every dispatched leg failed with a
// TRANSIENT verdict and a pool in the route unlocks within a short window, the
// gateway waits for that unlock and asks once instead of failing (magpie
// de73fa74: "two members each 429'd once, and the group asked the first one 16
// times while the one that could answer was never asked").
package gateway

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

func TestBriefRetryableClasses(t *testing.T) {
	yes := []string{uperr.ClassSoftRate, uperr.ClassModelRate}
	no := []string{
		uperr.ClassBadRequest, uperr.ClassHardCredit, uperr.ClassAccountRestricted,
		uperr.ClassQuotaExhausted, uperr.ClassCanceled, uperr.ClassNotFound,
		uperr.ClassUpstreamBusy, // its 429 carries Retry-After; the CLI resends itself (BP-169)
	}
	for _, c := range yes {
		if !isBriefRetryable(uperr.New(c, 429, "x")) {
			t.Fatalf("class %s must be brief-retryable", c)
		}
	}
	for _, c := range no {
		if isBriefRetryable(uperr.New(c, 400, "x")) {
			t.Fatalf("class %s must NOT be brief-retryable (long wall or request fault)", c)
		}
	}
	if isBriefRetryable(nil) {
		t.Fatal("nil must not be brief-retryable")
	}
}

// A nil trace must not panic the guard path.
func TestBriefRetryNilSafety(t *testing.T) {
	if isBriefRetryable(nil) {
		t.Fatal("nil error must not brief-retry")
	}
}

// The window constant: a cooldown further out than this is a real backoff the
// caller can do itself.
func TestBriefRetryWindowBounded(t *testing.T) {
	if briefRetryWindow <= 0 || briefRetryWindow > 5*time.Minute {
		t.Fatalf("briefRetryWindow = %v, want (0, 5m]", briefRetryWindow)
	}
}
