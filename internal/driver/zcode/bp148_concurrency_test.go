package zcode

// BP-148: ZCode 3008/3009 are concurrency backpressure, not an exhausted plan.
//
// ZCode answers 429 with code 3008 ("user concurrency limit exceeded") or 3009
// ("model concurrency limit exceeded") when one account has more in flight
// than its plan serves (magpie zcode 0.1.32 / #395). Before this, the code fell
// through to ClassInternal, and a bare 429 gave it a one-hour ResetAt — an hour
// out of service over a sub-second collision.

import (
	"testing"

	"bargainproxy/internal/uperr"
)

func TestConcurrencyCodesAreUpstreamBusy(t *testing.T) {
	for _, code := range []int{3008, 3009} {
		e := classifyBusiness(code, "concurrency limit exceeded", "glm-5.3")
		if e == nil {
			t.Fatalf("code %d produced no error", code)
		}
		if e.Class != uperr.ClassUpstreamBusy {
			t.Errorf("code %d class = %q, want %q", code, e.Class, uperr.ClassUpstreamBusy)
		}
		if e.Status != 429 {
			t.Errorf("code %d status = %d, want 429", code, e.Status)
		}
		if !e.ResetAt.IsZero() {
			t.Errorf("code %d carries ResetAt %v — a transient cap must not create a timed hold", code, e.ResetAt)
		}
		if e.Model == "" {
			t.Errorf("code %d should name the model so only that model's window pauses", code)
		}
	}
}

// TestConcurrencyRefusalDoesNotRotate: every account of the pool hitting the
// same model's concurrency ceiling means "wait", not "try the next account".
func TestConcurrencyRefusalDoesNotRotate(t *testing.T) {
	for _, code := range []int{3008, 3009} {
		e := classifyBusiness(code, "concurrency limit exceeded", "glm-5.3")
		if uperr.Rotatable(e.Class) {
			t.Errorf("code %d must not rotate accounts (class %q)", code, e.Class)
		}
	}
}

// TestQuotaCodesAreStillQuota is the boundary: the new branch must not swallow
// the real quota family next door (1005 / 1308+), which stays a 6h model hold.
func TestQuotaCodesAreStillQuota(t *testing.T) {
	for _, code := range []int{1005, 1308, 1321} {
		e := classifyBusiness(code, "quota exhausted", "glm-5.3")
		if e.Class != uperr.ClassSoftRate {
			t.Errorf("code %d class = %q, want %q", code, e.Class, uperr.ClassSoftRate)
		}
		if e.ResetAt.IsZero() {
			t.Errorf("code %d lost its ResetAt", code)
		}
	}
}

// TestUnknownCodeStillInternal: the default branch must keep behaving.
func TestUnknownCodeStillInternal(t *testing.T) {
	e := classifyBusiness(99999, "something else", "glm-5.3")
	if e.Class != uperr.ClassInternal {
		t.Fatalf("class = %q, want internal", e.Class)
	}
}
