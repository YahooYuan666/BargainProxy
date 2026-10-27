package workbuddy

// BP-145: a dead REFRESH token is not a dead ACCOUNT.
//
// Upstream answers 12153 to every refresh while the access token still works
// for weeks (misakano issue #83: 4 of 29 accounts, AT with 26-333 days left,
// /v2/chat/completions answering 200). The old path turned each one into a
// ClassAuth failure, and ClassAuth means a 10-minute pool rest — a permanent
// outage for an account that was serving traffic fine.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func credsWithExpiry(token string, exp time.Time) *Creds {
	return &Creds{AccessToken: token, RefreshToken: "rt-dead", ExpiresAt: exp.Unix()}
}

// TestAccessTokenUsableJudge pins the predicate: the access token's own clock
// decides, and an unknown clock never counts as usable (that would create a
// permanent zombie instead of a rest).
func TestAccessTokenUsableJudge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		creds *Creds
		want  bool
	}{
		{"weeks left", credsWithExpiry("at", now.Add(20*24*time.Hour)), true},
		{"minutes left", credsWithExpiry("at", now.Add(10*time.Minute)), true},
		{"about to expire", credsWithExpiry("at", now.Add(10*time.Second)), false},
		{"already expired", credsWithExpiry("at", now.Add(-time.Hour)), false},
		{"unknown expiry", &Creds{AccessToken: "at", RefreshToken: "rt"}, false},
		{"no access token", &Creds{RefreshToken: "rt", ExpiresAt: now.Add(24 * time.Hour).Unix()}, false},
	}
	for _, c := range cases {
		if got := c.creds.accessTokenUsable(now); got != c.want {
			t.Errorf("%s: accessTokenUsable = %v, want %v", c.name, got, c.want)
		}
	}
}

// newRefreshSrv answers the refresh route with a 12153 session-dead envelope,
// which is what these accounts see forever.
func newRefreshSrv(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":12153,"msg":"Session doesn't have required client"}`)
	}))
}

// TestDeadRefreshKeepsWorkingAccountInService is the bug: refreshing fails,
// but the account must stay usable and Refresh must return nil.
func TestDeadRefreshKeepsWorkingAccountInService(t *testing.T) {
	srv := newRefreshSrv(t)
	defer srv.Close()
	d := testDriver(srv.URL)

	deadline := time.Now().Add(30 * 24 * time.Hour) // ~a month of access token
	acc := accountWithCreds(fmt.Sprintf(
		`{"access_token":"at-good","refresh_token":"rt-dead","expires_at":%d,"uid":"u-1","realm":"cn"}`,
		deadline.Unix()))

	if err := d.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("a dead refresh token must not fail a live account: %v", err)
	}
	// The credentials must be untouched — nothing new arrived.
	if c, _ := ParseCreds(acc.Credentials()); c.AccessToken != "at-good" {
		t.Fatalf("credentials were rewritten: %+v", c)
	}
}

// TestDeadRefreshStillFailsDeadAccount is the other direction: when the access
// token is also gone, the refresh failure must surface (otherwise a truly dead
// account would stay selectable forever).
func TestDeadRefreshStillFailsDeadAccount(t *testing.T) {
	srv := newRefreshSrv(t)
	defer srv.Close()
	d := testDriver(srv.URL)

	acc := accountWithCreds(fmt.Sprintf(
		`{"access_token":"at-old","refresh_token":"rt-dead","expires_at":%d,"uid":"u-1","realm":"cn"}`,
		time.Now().Add(-2*time.Hour).Unix()))

	err := d.RefreshNow(context.Background(), acc)
	if err == nil {
		t.Fatalf("a dead account's failed refresh must surface")
	}
	if !strings.Contains(err.Error(), "refresh") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduledRefreshSkipsHealthyToken: the periodic refresher must remain a
// no-op while the token is fresh — otherwise BP-145 would fire a refresh every
// cycle and log noise for every healthy account.
func TestScheduledRefreshSkipsHealthyToken(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"data":{"accessToken":"new","refreshToken":"rt2","expiresIn":3600}}`)
	}))
	defer srv.Close()
	d := testDriver(srv.URL)

	acc := accountWithCreds(fmt.Sprintf(
		`{"access_token":"at-good","refresh_token":"rt","expires_at":%d,"uid":"u-1","realm":"cn"}`,
		time.Now().Add(48*time.Hour).Unix()))
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("scheduled refresh of a healthy token errored: %v", err)
	}
	if hits != 0 {
		t.Fatalf("scheduled refresh hit upstream %d times for a fresh token", hits)
	}
}
