package openaicompat

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-193 (magpie 3627742, issue #4095): a 400 carrying the credit-death
// wording is the balance death, not a client error — the pool must rotate to
// accounts that still have credit instead of ending the request.
func TestBP193CreditWordingOnA400IsHardCredit(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"可用积分已不足，请充值"}}`,
		`{"error":{"message":"积分已用完"}}`,
		`{"error":{"message":"out of credits"}}`,
		`{"error":{"message":"insufficient credit balance"}}`,
	} {
		e := classifyHTTP(400, []byte(body), "m")
		if e.Class != uperr.ClassHardCredit {
			t.Fatalf("400 + %q classified %s, want hard credit", body, e.Class)
		}
	}
	// A 400 with no credit wording stays a client error.
	if e := classifyHTTP(400, []byte(`{"error":{"message":"invalid parameter"}}`), "m"); e.Class != uperr.ClassBadRequest {
		t.Fatalf("a plain 400 must stay a bad request, got %s", e.Class)
	}
}

// BP-199 (magpie 3ec2552, plugins #468): a 401 whose body blames the MODEL
// (plan/entitlement) must not cool the account as an auth death.
func TestBP199ModelUnserved401IsNotAuthDeath(t *testing.T) {
	e := classifyHTTP(401, []byte(`{"error":{"message":"The model claude-opus-5 is not available on your plan"}}`), "m")
	if e.Class != uperr.ClassBadRequest {
		t.Fatalf("model-unserved 401 classified %s, want a request-level class", e.Class)
	}
	// A credential-shaped 401 stays auth.
	if e := classifyHTTP(401, []byte(`{"error":{"message":"invalid api key"}}`), "m"); e.Class != uperr.ClassAuth {
		t.Fatalf("a credential 401 must stay auth, got %s", e.Class)
	}
}

// BP-200 (magpie 750bd1b): a content-policy rejection wears 403 but is a
// request problem — ClassContentBlocked (no account penalty), not ClassWAF.
func TestBP200ContentPolicy403IsContentBlocked(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"content_policy_rejected","message":"flagged"}}`,
		`{"error":{"message":"content filter triggered"}}`,
		`{"error":{"message":"内容安全审查未通过"}}`,
	} {
		e := classifyHTTP(403, []byte(body), "m")
		if e.Class != uperr.ClassContentBlocked {
			t.Fatalf("403 + %q classified %s, want content blocked", body, e.Class)
		}
	}
	// A WAF-shaped 403 without policy wording stays WAF.
	if e := classifyHTTP(403, []byte(`{"error":{"message":"forbidden by edge"}}`), "m"); e.Class != uperr.ClassWAF {
		t.Fatalf("a plain 403 must stay WAF, got %s", e.Class)
	}
}
