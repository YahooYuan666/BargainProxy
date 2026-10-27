package qoder

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// The daily billing cap can arrive with its code hidden inside the message
// field (nested/escaped JSON) or as prose. It must reach ClassHardCredit with
// an overnight ResetAt instead of the generic 403 → ClassAuth path.
func TestBP152DailyBillingCapFallback(t *testing.T) {
	body := `{"code":-1,"message":"{\"code\":\"110\",\"message\":\"Billing daily count exceeded\",\"pricingUrl\":\"https://qoder.com/pricing\"}"}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassHardCredit {
		t.Fatalf("daily billing cap fell to %s, want hard_credit", e.Class)
	}
	if e.Code != "110" {
		t.Fatalf("code = %q, want 110", e.Code)
	}
	want := nextLocal04(time.Now())
	if diff := e.ResetAt.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("ResetAt = %s, want next local 04:00 (%s)", e.ResetAt, want)
	}
}

// The drained credit pool answers 403 with pricingUrl and no parseable
// top-level code. It must rest the account for the hub's 30 minutes, not be
// read as an auth failure.
func TestBP152CreditPoolFallback(t *testing.T) {
	e := classifyQoder(403, `{"message":"{\"code\":\"112\",\"pricingUrl\":\"https://www.qoder.com/pricing\"}"}`)
	if e.Class != uperr.ClassHardCredit {
		t.Fatalf("credit pool wall fell to %s, want hard_credit", e.Class)
	}
	if e.Code != "112" {
		t.Fatalf("code = %q, want 112", e.Code)
	}
	if diff := time.Until(e.ResetAt); diff < 25*time.Minute || diff > 35*time.Minute {
		t.Fatalf("ResetAt in %s, want ~30m", diff)
	}
}

// A plain 403 with no quota markers keeps taking the auth path.
func TestBP152Plain403StillAuth(t *testing.T) {
	e := classifyQoder(403, `{"message":"invalid token"}`)
	if e.Class != uperr.ClassAuth {
		t.Fatalf("plain 403 became %s, want auth", e.Class)
	}
	if !strings.Contains(e.Message, "invalid token") {
		t.Fatalf("message lost the body: %q", e.Message)
	}
}
