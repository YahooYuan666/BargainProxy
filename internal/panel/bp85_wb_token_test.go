package panel_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
)

// TestWBDeviceTokenIsNotLostWhenAccountLookupFails covers the 2026-10-21 field
// failure: the browser finishes the WorkBuddy login, the panel polls, the token
// endpoint issues the token ONCE, and the following account lookup fails. The
// old code dropped the token and every later poll asked upstream again, which
// answers "11217 login ing..." forever — the flow then timed out with
// 等待授权超时 and nothing was stored.
//
// The fix parks the issued token on the flow; a later poll must complete from
// it without touching the token endpoint again.
func TestWBDeviceTokenIsNotLostWhenAccountLookupFails(t *testing.T) {
	tokenHits := 0
	accountHits := 0
	wbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			fmt.Fprintf(w, `{"code":0,"msg":"","data":{"state":"st-9","authUrl":%q}}`, "https://www.workbuddy.ai/login?state=st-9")
		case "/v2/plugin/auth/token":
			tokenHits++
			fmt.Fprintf(w, `{"code":0,"msg":"","data":{"accessToken":%q,"refreshToken":%q,"expiresIn":3600,"domain":"workbuddy.ai"}}`,
				"at-9", "rt-9")
		case "/v2/plugin/login/account":
			accountHits++
			if accountHits == 1 {
				// First identity lookup fails (upstream hiccup / 401 HTML page).
				http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer at-9" {
				t.Errorf("account call must reuse the parked token, got %q", got)
			}
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"uid":"u-gl-9","nickname":"国际号"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer wbSrv.Close()

	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.WorkBuddy.ChatBaseCN = wbSrv.URL
		cfg.Channels.WorkBuddy.ChatBaseGL = wbSrv.URL
		cfg.WorkBuddyGlobal = true
	})
	cookie, csrf := loginHelper(t, srv)

	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/start", cookie, csrf, `{"realm":"global"}`)
	if resp.StatusCode != 200 {
		b := new(strings.Builder)
		_, _ = fmt.Fprint(b, resp.Body)
		resp.Body.Close()
		t.Fatalf("start status = %d body=%s", resp.StatusCode, b.String())
	}
	var start struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()

	body := fmt.Sprintf(`{"state":%q,"realm":"global"}`, start.State)

	// Poll 1: token issued, identity lookup fails -> still pending.
	p1 := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/poll", cookie, csrf, body)
	var r1 struct{ Status string }
	_ = json.NewDecoder(p1.Body).Decode(&r1)
	p1.Body.Close()
	if r1.Status != "pending" {
		t.Fatalf("poll1 = %+v, want pending", r1)
	}

	// Poll 2: must complete from the parked token, not from a second token GET.
	p2 := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/poll", cookie, csrf, body)
	var r2 struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.NewDecoder(p2.Body).Decode(&r2)
	p2.Body.Close()
	if r2.Status != "success" {
		t.Fatalf("poll2 = %+v, want success (token must survive the failed lookup)", r2)
	}
	if r2.AccountID != "workbuddy:u-gl-9" {
		t.Fatalf("account id = %q", r2.AccountID)
	}
	if tokenHits != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (the token is single-issue)", tokenHits)
	}
}
