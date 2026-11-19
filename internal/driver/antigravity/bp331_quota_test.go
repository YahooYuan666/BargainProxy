package antigravity

// BP-331 (magpie 2ba62f7f + c5f2a67d): a quota rejection can arrive as the
// MODEL'S OWN TEXT in a 200 stream — "Individual quota reached … Resets in
// 1h18m40s". The driver must synthesize the model-scoped soft rate instead of
// handing the rejection to the client as a normal answer.

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

func TestBP331BodyQuotaRejectSentinel(t *testing.T) {
	body := "Individual quota reached for model gemini-4-argon. Resets in 1h18m40s. Please try again later."
	e := bodyQuotaReject(body, "gemini-4-argon")
	if e == nil {
		t.Fatal("the sentinel must fire on the field wording")
	}
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate (model-scoped, no account penalty)", e.Class)
	}
	if e.Model != "gemini-4-argon" {
		t.Fatalf("e.Model = %q, want the model (rest lands in the model pool)", e.Model)
	}
	if d := time.Until(e.ResetAt); d < 70*time.Minute || d > 90*time.Minute {
		t.Fatalf("ResetAt = %v from now, want ~1h18m40s", d)
	}
}

// The second field wording fires too.
func TestBP331BodyQuotaRejectCapacityWording(t *testing.T) {
	e := bodyQuotaReject("You have exhausted your capacity on this model.", "m")
	if e == nil || e.Class != uperr.ClassSoftRate {
		t.Fatalf("capacity wording must fire: %v", e)
	}
	if !e.ResetAt.IsZero() {
		t.Fatal("no stated reset → the pool's ordinary soft backoff applies")
	}
}

// A real answer is not touched: no sentinel, or the text outgrew the window.
func TestBP331BodyQuotaRejectNegatives(t *testing.T) {
	if bodyQuotaReject("Here is the file you asked for, all 200 lines:", "m") != nil {
		t.Fatal("a normal answer must pass through")
	}
	long := strings.Repeat("x", bodyQuotaScanBytes+16) + " Individual quota reached"
	if bodyQuotaReject(long, "m") != nil {
		t.Fatal("past the scan window the check stops (a long real answer)")
	}
}
