package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// The old code posted {"refresh_token": ...} to /v2/plugin/auth/refresh,
// which does not exist upstream (404 Route Not Found, verified 2026-10-18),
// so a dead token could never be recovered. The live route is
// /v2/plugin/auth/token/refresh and takes the token in X-Refresh-Token.
func TestRefreshUsesRealEndpointAndHeader(t *testing.T) {
	var gotPath, gotHeader, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Get("X-Refresh-Token")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":5184000}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	acc := accountWithRefreshToken()
	// Force the refresh path: the scheduled refresher only fires near expiry.
	if err := d.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if gotPath != "/v2/plugin/auth/token/refresh" {
		t.Fatalf("refresh path = %q, want /v2/plugin/auth/token/refresh", gotPath)
	}
	if gotHeader != "test-refresh-value" {
		t.Fatalf("X-Refresh-Token = %q, want the stored refresh token", gotHeader)
	}
	if gotBody != "" {
		t.Fatalf("refresh body = %q, want empty (token travels in the header)", gotBody)
	}

	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		t.Fatalf("parse updated creds: %v", err)
	}
	if creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" {
		t.Fatalf("token not applied: access=%q refresh=%q", creds.AccessToken, creds.RefreshToken)
	}
	if !acc.CredsDirty {
		t.Fatal("CredsDirty must be set so the caller persists the new token")
	}
	// expiresIn 5184000 (60d) must move the deadline forward.
	if time.Until(time.Unix(creds.ExpiresAt, 0)) < 50*24*time.Hour {
		t.Fatalf("expires_at not extended: %s", time.Unix(creds.ExpiresAt, 0))
	}
}

// A scheduled refresh must stay cheap: a token that is not near expiry is not
// rotated at all.
func TestRefreshSkipsWhenNotNearExpiry(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"x"}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	acc := accountWithRefreshToken() // expires in 48h
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 (not near expiry)", hits)
	}
}

// RefreshNow exists precisely because a token can be revoked server-side while
// the local clock still says it is valid.
func TestRefreshNowForcesEvenWhenNotNearExpiry(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"forced","expiresIn":3600}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	acc := accountWithRefreshToken()
	if err := d.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	creds, _ := ParseCreds(acc.CredsJSON)
	if creds.AccessToken != "forced" {
		t.Fatalf("access token = %q, want forced", creds.AccessToken)
	}
}

// A flat {accessToken:...} body (no data envelope) must still parse.
func TestRefreshAcceptsBareBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accessToken":"bare","expiresIn":3600}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	acc := accountWithRefreshToken()
	if err := d.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	creds, _ := ParseCreds(acc.CredsJSON)
	if creds.AccessToken != "bare" {
		t.Fatalf("access token = %q, want bare", creds.AccessToken)
	}
	var raw map[string]any
	_ = json.Unmarshal(acc.CredsJSON, &raw)
	if raw["refresh_token"] != "test-refresh-value" {
		t.Fatalf("refresh token must be preserved when absent from the response: %v", raw["refresh_token"])
	}
}

// accountWithRefreshToken is testAccount plus the refresh token the real
// device-flow login always stores.
func accountWithRefreshToken() *model.Account {
	acc := testAccount()
	creds, _ := ParseCreds(acc.CredsJSON)
	creds.RefreshToken = fakeToken("refresh")
	raw, _ := json.Marshal(creds)
	acc.CredsJSON = raw
	return acc
}

// No refresh token stored: say so instead of posting an empty header.
func TestRefreshWithoutTokenFails(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"x"}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	err := d.RefreshNow(context.Background(), testAccount())
	if err == nil {
		t.Fatal("expected an error when no refresh token is stored")
	}
	if hits != 0 {
		t.Fatalf("upstream hits = %d, want 0", hits)
	}
}
