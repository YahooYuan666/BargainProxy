package autoclaw

import (
	"testing"
)

// BP-108: a banned AutoClaw account answers HTTP 403
// {"code":410004,"message":"The account has been banned"} on every route.
//
// Filing that as ClassAuth made the pool rest the account for 10 minutes, let
// the refresh task try, get the same account back, fail, and repeat forever —
// the account never left routing and the panel never said why. The correct
// class is ClassAccountRestricted, the terminal state workbuddy's 403/11140
// already uses: the account leaves routing and a human decides.
//
// Judged by code BEFORE the 401/403 fallback, and by wording too, because the
// ban also arrives without a code envelope (magpie-community/plugins 0.2.5).
func TestBannedAccountIsRestrictedNotAuth(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"code envelope", 403, `{"code":410004,"message":"The account has been banned"}`},
		{"code as string", 403, `{"code":"410004","message":"banned"}`},
		{"nested code", 403, `{"error":{"code":410004,"message":"The account has been banned"}}`},
		{"no envelope, english", 403, `The account has been banned`},
		{"no envelope, chinese", 403, `账号异常，已被封禁`},
		{"suspended wording", 403, `{"message":"account suspended"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ue := classifyHTTP(tc.status, []byte(tc.body))
			if ue.Class != "account_restricted" {
				t.Fatalf("%s: class = %s, want account_restricted (message=%q)",
					tc.name, ue.Class, ue.Message)
			}
		})
	}
}

// The ban must not swallow the neighbouring shapes: an ordinary expired
// sign-in still needs the refresh path, and a quota wall still needs the
// model-scoped hold.
func TestBanClassificationDoesNotOverreach(t *testing.T) {
	if ue := classifyHTTP(401, []byte(`{"msg":"invalid token"}`)); ue.Class != "auth" {
		t.Fatalf("expired sign-in became %s: the refresh path is lost", ue.Class)
	}
	if ue := classifyHTTP(403, []byte(`{"msg":"session expired, please sign in again"}`)); ue.Class != "auth" {
		t.Fatalf("expired sign-in became %s: the refresh path is lost", ue.Class)
	}
	if ue := classifyHTTP(402, []byte(`{"msg":"insufficient credit"}`)); ue.Class != "hard_credit" {
		t.Fatalf("quota wall became %s", ue.Class)
	}
	// 810000 is the per-route allowance wall and must stay model-scoped.
	if ue := classifyHTTP(403, []byte(`{"code":810000}`)); ue.Class != "hard_credit" {
		t.Fatalf("810000 became %s, want hard_credit", ue.Class)
	}
}
