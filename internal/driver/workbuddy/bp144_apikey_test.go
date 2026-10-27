package workbuddy

// BP-144: WorkBuddy API-key accounts.
//
// A key minted on www.workbuddy.cn/profile/keys signs in the way
// @tencent-ai/codebuddy-code does with CODEBUDDY_API_KEY (magpie workbuddy
// 0.1.37 / #407): the key is both the bearer and X-API-Key, the identity is
// "anonymous_" + the key's last 8 characters, the account is personal with no
// domain and no enterprise, and nothing can be renewed.
//
// Before this, a key pasted as an access token produced three wrong shapes at
// once — no X-API-Key, an X-Domain the CLI never sends — and a 401 triggered a
// refresh of something that cannot be refreshed, cooling a working account.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

func TestAPIKeyUserIDIsLastEight(t *testing.T) {
	if got := apiKeyUserID("ck_1234567890abcdef"); got != "anonymous_90abcdef" {
		t.Fatalf("apiKeyUserID = %q, want anonymous_90abcdef", got)
	}
	// Short keys cannot be sliced by the last 8; they must not panic.
	if got := apiKeyUserID("short"); got != "anonymous_short" {
		t.Fatalf("short key = %q", got)
	}
}

func TestRemoteCatalogKeyOnlyForCKAndPT(t *testing.T) {
	for _, k := range []string{"ck_abcdefgh", "pt_abcdefgh"} {
		if !hasRemoteCatalogKey(k) {
			t.Fatalf("%q should be allowed to ask /v3/config", k)
		}
	}
	for _, k := range []string{"sk_abcdefgh", "abcdefgh", ""} {
		if hasRemoteCatalogKey(k) {
			t.Fatalf("%q must not ask /v3/config", k)
		}
	}
}

// TestKeyAccountSendsBothAuthHeaders is the core of the fix: the upstream
// expects the key twice.
func TestKeyAccountSendsBothAuthHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	applyAuth(req, &Creds{APIKey: "ck_testkey0000abcd"})
	if got := req.Header.Get("X-API-Key"); got != "ck_testkey0000abcd" {
		t.Fatalf("X-API-Key = %q, want the key", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ck_testkey0000abcd" {
		t.Fatalf("Authorization = %q, want Bearer <key>", got)
	}
}

// TestOAuthAccountSendsNoAPIKeyHeader: an OAuth account must not carry the
// header at all (its absence is part of the client's shape).
func TestOAuthAccountSendsNoAPIKeyHeader(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	applyAuth(req, &Creds{AccessToken: "at-token"})
	if got := req.Header.Get("X-API-Key"); got != "" {
		t.Fatalf("OAuth account sent X-API-Key: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestKeyAccountOmitsDomainAndEnterprise: "no domain, no enterprise" is the
// CLI's own description of a key session.
func TestKeyAccountOmitsDomainAndEnterprise(t *testing.T) {
	d := &Driver{}
	req, _ := http.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	creds := &Creds{APIKey: "ck_testkey0000abcd", UID: "a-real-uid", EnterpriseID: "ent-1"}
	d.applyClientFingerprint(req, creds, driver.ChatCall{Request: &ir.ChatRequest{Model: "glm-5.3"}})

	if got := req.Header.Get("X-Domain"); got != "" {
		t.Fatalf("key account sent X-Domain: %q", got)
	}
	if got := req.Header.Get("X-User-Id"); got != apiKeyUserID(creds.APIKey) {
		t.Fatalf("X-User-Id = %q, want the anonymous key identity", got)
	}
}

// TestOAuthAccountStillSendsDomain pins the other direction, so the fix
// cannot silently strip the domain from the accounts that need it.
func TestOAuthAccountStillSendsDomain(t *testing.T) {
	d := &Driver{}
	req, _ := http.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	d.applyClientFingerprint(req, &Creds{AccessToken: "at", UID: "u1"},
		driver.ChatCall{Request: &ir.ChatRequest{Model: "glm-5.3"}})
	if got := req.Header.Get("X-Domain"); got == "" {
		t.Fatalf("OAuth account lost its X-Domain")
	}
}

// TestKeyAccountCannotRefresh: refreshing a key spends an RTT and converts a
// working account into a ClassAuth cooldown.
func TestKeyAccountCannotRefresh(t *testing.T) {
	acc := accountWithCreds(`{"api_key":"ck_abcdefgh1234"}`)
	d := &Driver{}
	err := d.refresh(context.Background(), acc, true)
	if err == nil {
		t.Fatalf("refreshing a key account was allowed")
	}
	if !strings.Contains(err.Error(), "cannot refresh") {
		t.Fatalf("unexpected error: %v", err)
	}
	// It must be ClassAuth so the pool does not treat it as transient.
	if !IsAuthDead(err) {
		t.Fatalf("key refresh failure should classify as auth-dead: %v", err)
	}
}

// TestKeyAccountProbeIsNotAnError: unknown credits are not a fault.
func TestKeyAccountProbeIsNotAnError(t *testing.T) {
	acc := accountWithCreds(`{"api_key":"ck_abcdefgh1234"}`)
	d := &Driver{}
	res, err := d.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("probe of a key account failed: %v", err)
	}
	if res == nil || res.Credits == nil || !res.Credits.BalanceOnly {
		t.Fatalf("probe should report unknown balance (BalanceOnly), got %+v", res)
	}
	if !strings.Contains(res.Note, "not reported") {
		t.Fatalf("probe note should explain why, got %q", res.Note)
	}
}

// TestKeyShapeSurvivesJSONRoundTrip: the panel stores credentials as JSON.
func TestKeyShapeSurvivesJSONRoundTrip(t *testing.T) {
	raw := []byte(`{"api_key":"ck_abcdefgh1234","realm":"cn"}`)
	c, err := ParseCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsAPIKey() {
		t.Fatalf("api_key did not parse into IsAPIKey()")
	}
	// A legacy account must NOT look like a key account.
	c2, _ := ParseCreds([]byte(`{"access_token":"at","refresh_token":"rt"}`))
	if c2.IsAPIKey() {
		t.Fatalf("OAuth credentials parsed as a key account")
	}
}

// httptest is imported for the driver's own HTTP paths in neighbouring tests;
// keep the reference so a future edit does not silently drop it.
var _ = httptest.NewRecorder

// accountWithCreds builds an account carrying the given credential JSON, the
// way the panel stores them.
func accountWithCreds(raw string) *model.Account {
	return &model.Account{ID: "acc-key", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(raw)}
}
