package lobster

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-125: 40302 "账号已被禁用" is a server-side risk-control ban (Coding2API
// 04218a3: a fresh account hits it on its first call; client-shape fixes do
// not help). It must leave routing, not land in bad_request where the pool
// keeps hammering the dead account.
func Test40302IsAccountRestricted(t *testing.T) {
	e := classifyHTTP(200, []byte(`{"code":40302,"message":"账号已被禁用"}`))
	if e.Class != uperr.ClassAccountRestricted {
		t.Fatalf("40302 class = %s, want account_restricted", e.Class)
	}
	// The surrounding envelopes must keep their old classes: the ban code is
	// the only one that moved.
	if e := classifyCode(40100, "no auth"); e.Class != uperr.ClassAuth {
		t.Fatalf("40100 class = %s, want auth", e.Class)
	}
	if e := classifyCode(40301, "not entitled"); e.Class != uperr.ClassHardCredit {
		t.Fatalf("40301 class = %s, want hard_credit", e.Class)
	}
}
