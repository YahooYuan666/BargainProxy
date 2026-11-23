// BP-335: trae risk-control 403 must stop rotation. 4015 (upstream rejected)
// and 4019 (risk hit) are decided by the DEVICE FINGERPRINT and egress IP,
// which every account in the pool shares — the next account gets the identical
// answer. The old path (unrecognised code → ClassInternal → rotatable) walked
// the whole pool charging each account a strike; the upstream reference
// (trae2api-hub 683a3385, 2026-11-20) measured ONE request leaving the pool
// unusable for 30 minutes.
package trae

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

func TestRiskControlCodesAreNotRotatable(t *testing.T) {
	for _, code := range []int{4015, 4019} {
		e := classifyBusiness(code, "risk control hit")
		if e.Class != uperr.ClassAccountRestricted {
			t.Fatalf("code %d class = %s, want %s", code, e.Class, uperr.ClassAccountRestricted)
		}
		if uperr.Rotatable(e.Class) {
			t.Fatalf("code %d must NOT be rotatable — walking the pool leaves a risk record on every account", code)
		}
		if e.Code != itoa(code) {
			t.Fatalf("code %d e.Code = %q, want %q (panel reads it)", code, e.Code, itoa(code))
		}
		if ttl := time.Until(e.ResetAt); ttl <= 0 || ttl > 45*time.Minute {
			t.Fatalf("code %d ResetAt ttl = %v, want about 30 minutes (device flag clears fast)", code, ttl)
		}
	}
}

// The upstream's risk-control copy must not be forwarded verbatim: it names
// internal signals and reads as a user-facing accusation.
func TestRiskControlWordingIsNotForwarded(t *testing.T) {
	e := classifyBusiness(4019, "device_risk_score=97 unusual activity detected on account")
	if strings.Contains(e.Message, "device_risk_score") || strings.Contains(e.Message, "97") {
		t.Fatalf("risk-control internals leaked into the client message: %q", e.Message)
	}
}

// A 403 with no parseable business code can still be a risk hit — the upstream
// sometimes words it without a numeric code.
func TestRiskControl403WithoutCode(t *testing.T) {
	for _, body := range []string{
		`{"message":"risk_control"} `,
		`{"msg":"unusual activity detected"}`,
		`{"message":"Risk Control triggered"}`,
	} {
		e := classifyHTTP(403, []byte(body), "deepseek-v4.1-flash")
		if e.Class != uperr.ClassAccountRestricted {
			t.Fatalf("body %q class = %s, want %s", body, e.Class, uperr.ClassAccountRestricted)
		}
	}
}

// Ordinary auth/session 403s must still rotate: only risk control is
// device-scoped.
func TestOrdinary403StillRotates(t *testing.T) {
	e := classifyHTTP(403, []byte(`{"message":"token expired"}`), "")
	if e.Class == uperr.ClassAccountRestricted {
		t.Fatal("a plain 403 must not be read as a risk-control hit")
	}
	if !uperr.Rotatable(e.Class) {
		t.Fatalf("a plain 403 must stay rotatable, got class %s", e.Class)
	}
}

// Digit adjacency: 14019 / 40150 are ordinary codes and must not be caught.
func TestRiskCodeAdjacencyGuard(t *testing.T) {
	for _, code := range []int{14019, 40150, 401, 403} {
		e := classifyBusiness(code, "x")
		if e.Class == uperr.ClassAccountRestricted {
			t.Fatalf("code %d must not be read as a risk-control hit", code)
		}
	}
}

func itoa(n int) string {
	if n == 4015 {
		return "4015"
	}
	return "4019"
}
