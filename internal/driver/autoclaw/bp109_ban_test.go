package autoclaw

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-109: a banned AutoClaw account answers 403 {"code":410004,"message":"The
// account has been banned"} on EVERY route. That is not an expired sign-in:
// refreshing gets the same account back with the same refusal. Filing it as
// ClassAuth made the pool rest the account for 10 minutes, let the refresh
// task try and fail, and repeat forever — while the panel never said "banned".
//
// ClassAccountRestricted is the terminal state the pool already understands
// (workbuddy's 403/11140 uses it): the account leaves routing and a human
// decides.
func TestBannedCodeIsAccountRestricted(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"json envelope", 403, `{"code":410004,"message":"The account has been banned"}`},
		{"numeric code", 403, `{"code":410004}`},
		{"string code", 403, `{"code":"410004"}`},
		{"nested error", 403, `{"error":{"code":410004,"message":"account banned"}}`},
		{"401 shape", 401, `{"code":410004,"message":"The account has been banned"}`},
		// Wording only, no code envelope: the same refusal stated in words.
		{"wording en", 403, `{"message":"The account has been banned"}`},
		{"wording cn", 403, `{"message":"账号异常，已被限制使用"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classifyHTTP(tc.status, []byte(tc.body))
			if e.Class != uperr.ClassAccountRestricted {
				t.Fatalf("class = %q, want %q (a ban must not be filed as a dead sign-in, or the pool refreshes it forever)",
					e.Class, uperr.ClassAccountRestricted)
			}
			if e.Status != 403 {
				t.Fatalf("status = %d, want 403", e.Status)
			}
		})
	}
}

// The neighbours must not move: a genuinely expired session still needs the
// auth path (so the refresh task gets its chance), and an allowance wall is
// still a per-model 402.
func TestBanClassificationDoesNotSwallowNeighbours(t *testing.T) {
	if e := classifyBusiness(410000, "session dead"); e.Class != uperr.ClassAuth {
		t.Fatalf("410000 session dead class = %q, want auth", e.Class)
	}
	if e := classifyHTTP(401, []byte(`{"message":"token expired"}`)); e.Class != uperr.ClassAuth {
		t.Fatalf("expired token class = %q, want auth", e.Class)
	}
	if e := classifyHTTP(402, []byte(`{"msg":"insufficient credit"}`)); e.Class != uperr.ClassHardCredit {
		t.Fatalf("credit wall class = %q, want hard_credit", e.Class)
	}
	if e := classifyHTTP(429, []byte(`too many`)); e.Class != uperr.ClassSoftRate {
		t.Fatalf("throttle class = %q, want soft_rate", e.Class)
	}
}
