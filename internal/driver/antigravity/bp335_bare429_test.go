// BP-335: a "bare" 429 is REQUEST-scoped, not account-scoped. Google answers
// it both for a throttled account and for a single request it refuses to
// serve; in the second case the same body gets the same 429 from every account
// it reaches. Cooling each one walks the pool offline for everyone.
//
// sub2api #8973 field evidence: 2026-11-16 one 1.8MB request bare-429'd four
// accounts in a row while those same accounts served other requests normally;
// 2026-11-19 a 2MB Codex request cooled three accounts for a minute.
package antigravity

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// A bare 429 — no reset moment, no ErrorInfo, no details — is request-scoped.
func TestBare429IsRequestScoped(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)
	e := classifyHTTP(429, body, "gemini-3.8-flash-high")
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate", e.Class)
	}
	if !e.RequestScoped {
		t.Fatal("a bare 429 (no reset, no details) must be marked request-scoped")
	}
}

// A 429 that names WHEN it resets is a real per-account window: cooling it is
// correct and must not be skipped.
func TestResetBearing429IsNotRequestScoped(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"537s"}]}}`)
	e := classifyHTTP(429, body, "gemini-3.8-flash-high")
	if e.RequestScoped {
		t.Fatal("a 429 carrying a reset delay must NOT be request-scoped")
	}
	if e.ResetAt.IsZero() {
		t.Fatal("the reset delay must be parsed into ResetAt")
	}
}

// An ErrorInfo-bearing 429 explains itself per account; leave it alone.
func TestErrorInfo429IsNotRequestScoped(t *testing.T) {
	body := []byte(`{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","domain":"cloudcode-pa.googleapis.com"}]}}`)
	e := classifyHTTP(429, body, "m")
	if e.RequestScoped {
		t.Fatal("a 429 carrying ErrorInfo must NOT be request-scoped")
	}
}

// Non-429 failures are never request-scoped by this rule.
func TestNon429NeverRequestScoped(t *testing.T) {
	for _, st := range []int{400, 403, 500, 503} {
		e := classifyHTTP(st, []byte(`{"error":{"message":"x"}}`), "m")
		if e.RequestScoped {
			t.Fatalf("status %d must not be request-scoped", st)
		}
	}
}

// The reset parse itself must still work (guards the guard).
func TestResetDelayStillParsed(t *testing.T) {
	got := parseQuotaReset(`{"retryDelay":"120s"}`)
	if got.IsZero() {
		t.Fatal("retryDelay must parse")
	}
	if d := time.Until(got); d < 100*time.Second || d > 140*time.Second {
		t.Fatalf("retryDelay ttl = %v, want about 120s", d)
	}
}
