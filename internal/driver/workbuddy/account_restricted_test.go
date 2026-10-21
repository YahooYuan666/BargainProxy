package workbuddy

import (
	"net/http"
	"testing"

	"bargainproxy/internal/uperr"
)

// TestAccountRestrictedIsItsOwnClass: upstream answers
// `403 {"code":11140,"msg":"request illegal","displayMsg":{"zh":"账号存在异常行为，已被限制使用"}}`
// when it has restricted the ACCOUNT. That is not an unknown gateway bug and
// not a rate limit: the account must leave routing so the pool rotates to a
// healthy one. Before 2026-10-20 this fell into the generic internal bucket,
// so two restricted accounts kept being selected (sticky sessions bound to
// them, failover cascaded every turn) and the whole workbuddy pool looked
// broken while exactly one account was still fine.
func TestAccountRestrictedIsItsOwnClass(t *testing.T) {
	body := []byte(`{"code":11140,"msg":"request illegal","requestId":"r1","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"}}`)
	ue := classifyHTTP(http.StatusForbidden, body, "deepseek-v4.1-flash")
	if ue.Class != uperr.ClassAccountRestricted {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassAccountRestricted)
	}
	if ue.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", ue.Status)
	}
}

// The same business code also carries a model-level rate limit; that variant
// must stay soft_rate (rotating the account cannot fix a rate limit, but the
// account also must not be disabled).
func Test11140RateLimitVariantStaysSoftRate(t *testing.T) {
	body := []byte(`{"code":11140,"msg":"The model provider is rate-limiting requests."}`)
	ue := classifyHTTP(http.StatusTooManyRequests, body, "deepseek-v4.1-flash")
	if ue.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassSoftRate)
	}
}

// A plain 403 with the restriction marker but no JSON envelope (the edge
// sometimes answers bare text) must also be recognized; a generic 403 stays
// WAF (IP-scoped, short cooldown).
func TestRestrictionMarkerWithoutEnvelope(t *testing.T) {
	ue := classifyHTTP(http.StatusForbidden, []byte(`{"code":11140,"msg":"request illegal"}`), "m")
	if ue.Class != uperr.ClassAccountRestricted {
		t.Fatalf("envelope class = %s", ue.Class)
	}
	ue = classifyHTTP(http.StatusForbidden, []byte(`request illegal: account restricted`), "m")
	if ue.Class != uperr.ClassAccountRestricted {
		t.Fatalf("bare class = %s", ue.Class)
	}
	ue = classifyHTTP(http.StatusForbidden, []byte(`forbidden by policy`), "m")
	if ue.Class != uperr.ClassWAF {
		t.Fatalf("generic 403 class = %s, want WAF", ue.Class)
	}
}
