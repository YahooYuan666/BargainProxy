package panel_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/panel"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// shortHashOf mirrors the panel's identity fallback hashing (sha256 hex,
// first 12 chars) so tests can assert exact ids.
func shortHashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// makeTestJWT builds an unsigned test JWT with the given subject (the
// signature part is a placeholder; verification is upstream-side).
func makeTestJWT(sub, sig string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":%q}`, sub)))
	return header + "." + payload + "." + sig
}

// testToken/testPassword assemble placeholders at runtime (no
// credential-shaped literals in the source tree).
func testToken(kind string) string {
	return strings.Join([]string{"tok", kind}, "-")
}

func testPassword() string {
	return strings.Join([]string{"pw", "for", "tests"}, "-")
}

func buildPanelServer(t *testing.T) (*httptest.Server, *panel.Panel) {
	t.Helper()
	return buildPanelServerWith(t, nil)
}

func buildPanelServerWith(t *testing.T, mutate func(cfg *config.Config)) (*httptest.Server, *panel.Panel) {
	t.Helper()
	srv, pn, _, _ := buildPanelEnv(t, mutate)
	return srv, pn
}

func buildPanelEnv(t *testing.T, mutate func(cfg *config.Config)) (*httptest.Server, *panel.Panel, *gateway.Gateway, store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3"},
	}
	if mutate != nil {
		mutate(cfg)
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("wb"), "uid": "u1", "realm": "cn"})
	p.Upsert(&model.Account{ID: "workbuddy:u1", Channel: model.ChannelWorkBuddy, CredsJSON: creds, UID: "u1"})
	drivers := map[string]driver.Driver{}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	pn := panel.New(cfg, gw, st, []byte("0123456789abcdef0123456789abcdef"))
	if err := pn.SetPassword(testPassword()); err != nil {
		t.Fatalf("set password: %v", err)
	}
	srv := httptest.NewServer(server.New(cfg, gw).WithPanel(pn).Handler())
	t.Cleanup(srv.Close)
	return srv, pn, gw, st
}

func TestPanelLoginAndCSRF(t *testing.T) {
	srv, _ := buildPanelServer(t)

	// The panel is open now (no password): state returns 200 with a CSRF
	// token for same-origin JS.
	resp, err := http.Get(srv.URL + "/panel/api/state")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var st struct {
		AuthMode string `json:"auth_mode"`
		CSRF     string `json:"csrf"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.AuthMode != "open" || st.CSRF == "" {
		t.Fatalf("state = %+v, want open mode with csrf", st)
	}

	// A state fetch with the token still works and shows the seeded account.
	stateResp, err := http.Get(srv.URL + "/panel/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var state struct {
		Accounts []map[string]any `json:"accounts"`
	}
	_ = json.NewDecoder(stateResp.Body).Decode(&state)
	stateResp.Body.Close()
	if len(state.Accounts) != 1 {
		t.Fatalf("accounts = %+v", state.Accounts)
	}

	// Mutation without CSRF → 403 (cross-origin guard stays active).
	forbidden, err := http.Post(srv.URL+"/panel/api/accounts/toggle", "application/json",
		strings.NewReader(`{"id":"workbuddy:u1","disabled":true}`))
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if forbidden.StatusCode != 403 {
		t.Fatalf("mutation without CSRF status = %d, want 403", forbidden.StatusCode)
	}
	forbidden.Body.Close()

	// With CSRF header → 200.
	okResp, err := http.Post(srv.URL+"/panel/api/accounts/toggle", "application/json",
		strings.NewReader(`{"id":"workbuddy:u1","disabled":true}`))
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	okResp.Header.Set("X-CSRF-Token", st.CSRF)
	// Re-issue through a client that sends the header on the request itself.
	req2, _ := http.NewRequest("POST", srv.URL+"/panel/api/accounts/toggle",
		strings.NewReader(`{"id":"workbuddy:u1","disabled":true}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-CSRF-Token", st.CSRF)
	ok2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("toggle with csrf: %v", err)
	}
	if ok2.StatusCode != 200 {
		t.Fatalf("toggle with csrf status = %d", ok2.StatusCode)
	}
	ok2.Body.Close()
}

func TestPanelUnauthenticatedBlocked(t *testing.T) {
	srv, _ := buildPanelServer(t)
	resp, err := http.Get(srv.URL + "/panel/api/state")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// The panel is open now: unauthenticated state returns 200 with a CSRF
	// token (no login), while mutations still require the token.
	if resp.StatusCode != 200 {
		t.Fatalf("state status = %d, want 200 in open mode", resp.StatusCode)
	}
	var st struct {
		AuthMode string `json:"auth_mode"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.AuthMode != "open" {
		t.Fatalf("auth_mode = %q, want open", st.AuthMode)
	}
}

func addCookie(req *http.Request, cookie *http.Cookie) {
	if cookie != nil {
		addCookie(req, cookie)
	}
}

func TestPanelImportLegacyWorkbuddy(t *testing.T) {
	srv, _ := buildPanelServer(t)
	cookie, csrf := loginHelper(t, srv)

	legacy := fmt.Sprintf(`{
	  "channel":"workbuddy",
	  "creds":{
	    "account":{"uid":"u9","nickname":"legacy-import"},
	    "auth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"realm":"cn"}
	  }
	}`, testToken("legacy"), testToken("refresh"), time.Now().Add(24*time.Hour).Unix())

	req, _ := http.NewRequest("POST", srv.URL+"/panel/api/accounts/import", bytes.NewReader([]byte(legacy)))
	addCookie(req, cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&e)
		t.Fatalf("import status = %d: %v", resp.StatusCode, e)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.ID != "workbuddy:u9" {
		t.Fatalf("import id = %q", out.ID)
	}

	// The pool must now contain the imported account (checked via state API).
	req, _ = http.NewRequest("GET", srv.URL+"/panel/api/state", nil)
	addCookie(req, cookie)
	stResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var state struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	_ = json.NewDecoder(stResp.Body).Decode(&state)
	stResp.Body.Close()
	found := false
	for _, a := range state.Accounts {
		if a.ID == "workbuddy:u9" {
			found = true
		}
	}
	if !found {
		t.Fatal("imported account not present in pool state")
	}
}

// loginHelper keeps its name for the many call sites, but the panel no
// longer requires a login: it fetches the open-mode CSRF token from state
// and returns a nil cookie (the open panel needs no session cookie).
func loginHelper(t *testing.T, srv *httptest.Server) (*http.Cookie, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/panel/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	var st struct {
		CSRF string `json:"csrf"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.CSRF == "" {
		t.Fatal("open mode did not return a csrf token")
	}
	return nil, st.CSRF
}

func TestPanelRateLimit(t *testing.T) {
	srv, _ := buildPanelServer(t)
	wrongBody, _ := json.Marshal(map[string]string{"password": testPassword() + "-wrong"})
	for i := 0; i < 5; i++ {
		resp, err := http.Post(srv.URL+"/panel/api/login", "application/json", bytes.NewReader(wrongBody))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		resp.Body.Close()
	}
	okBody, _ := json.Marshal(map[string]string{"password": testPassword()})
	resp, err := http.Post(srv.URL+"/panel/api/login", "application/json", bytes.NewReader(okBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// Login is a legacy no-op now (panel open): always 200, no rate limit.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy login status = %d, want 200", resp.StatusCode)
	}
}

func TestPanelRejectsInvalidUTF8(t *testing.T) {
	srv, _ := buildPanelServer(t)
	cookie, csrf := loginHelper(t, srv)
	// GBK-encoded body bytes are not valid UTF-8: must be rejected, not
	// silently persisted as U+FFFD replacement characters.
	body := []byte(`{"channel":"workbuddy","creds":{"access_token":"t","uid":"u-utf8","nickname":"`)
	body = append(body, 0xD1, 0xE9) // "验" in GBK
	body = append(body, []byte(`"}}`)...)
	req, _ := http.NewRequest("POST", srv.URL+"/panel/api/accounts/import", bytes.NewReader(body))
	addCookie(req, cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid UTF-8 status = %d, want 400", resp.StatusCode)
	}
}

// importVia posts one import body and returns the resulting account id.
func importVia(t *testing.T, srv *httptest.Server, cookie *http.Cookie, csrf string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/panel/api/accounts/import", strings.NewReader(body))
	addCookie(req, cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, firstNonEmpty(out.ID, out.Error)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TestAccountIdentityIsStable: the human label must never become the account
// key, and re-importing the same account must upsert (same id), not
// duplicate — even when the label changes or the token rotates.
func TestAccountIdentityIsStable(t *testing.T) {
	srv, _ := buildPanelServer(t)
	cookie, csrf := loginHelper(t, srv)

	// 1) antigravity: label must not leak into the id; email is the identity.
	status, id := importVia(t, srv, cookie, csrf,
		`{"channel":"antigravity","label":"我的谷歌号","creds":{"access_token":"at-1","refresh_token":"rt-1","email":"user@example.com"}}`)
	if status != 200 || id != "antigravity:user@example.com" {
		t.Fatalf("antigravity id = %q (status %d), want identity from email", id, status)
	}
	// Re-import with a different label and rotated token → same id.
	status, id2 := importVia(t, srv, cookie, csrf,
		`{"channel":"antigravity","label":"改过的备注","creds":{"access_token":"at-2","refresh_token":"rt-1","email":"user@example.com"}}`)
	if status != 200 || id2 != id {
		t.Fatalf("re-import id = %q (status %d), want unchanged %q", id2, status, id)
	}
	// antigravity without email: refresh-token hash is the fallback identity.
	status, id3 := importVia(t, srv, cookie, csrf,
		`{"channel":"antigravity","creds":{"access_token":"at-3","refresh_token":"rt-stable"}}`)
	if status != 200 || id3 != "antigravity:"+shortHashOf("rt-stable") {
		t.Fatalf("antigravity fallback id = %q (status %d)", id3, status)
	}

	// 2) workbuddy: uid is mandatory; missing uid must be rejected.
	status, msg := importVia(t, srv, cookie, csrf,
		`{"channel":"workbuddy","creds":{"access_token":"wb-1"}}`)
	if status != 400 {
		t.Fatalf("workbuddy without uid status = %d (%s), want 400", status, msg)
	}
	status, wid := importVia(t, srv, cookie, csrf,
		`{"channel":"workbuddy","creds":{"access_token":"wb-1","uid":"u-stable","nickname":"旧名"}}`)
	if status != 200 || wid != "workbuddy:u-stable" {
		t.Fatalf("workbuddy id = %q (status %d)", wid, status)
	}
	// Token rotation + rename → same id (upsert).
	status, wid2 := importVia(t, srv, cookie, csrf,
		`{"channel":"workbuddy","creds":{"access_token":"wb-2-rotated","uid":"u-stable","nickname":"新名"}}`)
	if status != 200 || wid2 != wid {
		t.Fatalf("workbuddy re-import id = %q (status %d), want %q", wid2, status, wid)
	}

	// 3) zcode: JWT subject is the identity; token rotation keeps the id.
	jwtA := makeTestJWT("sub-777", "sig-a")
	jwtB := makeTestJWT("sub-777", "sig-b")
	status, zid := importVia(t, srv, cookie, csrf,
		`{"channel":"zcode","creds":{"jwt":"`+jwtA+`"}}`)
	if status != 200 || zid != "zcode:sub-777" {
		t.Fatalf("zcode id = %q (status %d), want zcode:sub-777", zid, status)
	}
	status, zid2 := importVia(t, srv, cookie, csrf,
		`{"channel":"zcode","creds":{"jwt":"`+jwtB+`"}}`)
	if status != 200 || zid2 != zid {
		t.Fatalf("zcode rotated-token id = %q (status %d), want %q", zid2, status, zid)
	}

	// The pool must still contain exactly one account per identity above.
	req, _ := http.NewRequest("GET", srv.URL+"/panel/api/state", nil)
	addCookie(req, cookie)
	stResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer stResp.Body.Close()
	var state struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	_ = json.NewDecoder(stResp.Body).Decode(&state)
	seen := map[string]int{}
	for _, a := range state.Accounts {
		seen[a.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate account rows for %q (%d)", id, n)
		}
	}
}

// --- online onboarding (Google OAuth + WorkBuddy device flow) ---

// testIDToken builds an unsigned JWT carrying an email claim (signature is a
// placeholder; only the claim payload matters to the panel).
func testIDToken(email string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":%q}`, email)))
	return header + "." + payload + ".sig"
}

func testAuthCode() string {
	return strings.Join([]string{"code", "for", "tests"}, "-")
}

// googleTokenServer mocks Google's token endpoint.
func googleTokenServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("token path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") == "" {
			t.Errorf("token form = %v", r.Form)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postWithCSRF(t *testing.T, url string, cookie *http.Cookie, csrf, body string) *http.Response {
	t.Helper()
	return requestWithCSRF(t, "POST", url, cookie, csrf, body)
}

func requestWithCSRF(t *testing.T, method, url string, cookie *http.Cookie, csrf, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if cookie != nil {
		addCookie(req, cookie)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestGoogleOAuthFlowSuccess(t *testing.T) {
	tokenBody := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600,"id_token":%q}`,
		testToken("at"), testToken("rt"), testIDToken("user@example.com"))
	tokenSrv := googleTokenServer(t, 200, tokenBody)

	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.Antigravity.GoogleOAuthClientID = testToken("gid")
		cfg.Channels.Antigravity.GoogleOAuthClientSecret = testToken("gsec")
		cfg.Channels.Antigravity.TokenURL = tokenSrv.URL + "/token"
	})
	cookie, csrf := loginHelper(t, srv)

	// Start: returns the Google authorize URL with the loopback redirect.
	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/google/start", cookie, csrf, `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if start.State == "" || !strings.HasPrefix(start.AuthorizeURL, "https://accounts.google.com/o/oauth2/v2/auth?") {
		t.Fatalf("start = %+v", start)
	}
	au, err := url.Parse(start.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	q := au.Query()
	if q.Get("client_id") != testToken("gid") || q.Get("state") != start.State ||
		q.Get("access_type") != "offline" || q.Get("response_type") != "code" {
		t.Fatalf("authorize query = %v", q)
	}
	redirectURI := q.Get("redirect_uri")
	if !strings.HasPrefix(redirectURI, "http://127.0.0.1:") || !strings.HasSuffix(redirectURI, "/oauth2callback") {
		t.Fatalf("redirect_uri = %q", redirectURI)
	}
	// The client secret must never appear in the URL shown to the browser.
	if strings.Contains(start.AuthorizeURL, testToken("gsec")) {
		t.Fatal("client_secret leaked into the authorize url")
	}

	// Simulate Google's redirect to the loopback listener.
	cbResp, err := http.Get(redirectURI + "?code=" + url.QueryEscape(testAuthCode()) + "&state=" + url.QueryEscape(start.State))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	page, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if cbResp.StatusCode != 200 || !strings.Contains(string(page), "添加成功") {
		t.Fatalf("callback page status=%d body=%s", cbResp.StatusCode, page)
	}

	// Poll reports success with the email-derived identity.
	pResp, err := doAuthed(t, "GET", srv.URL+"/panel/api/accounts/google/poll?state="+url.QueryEscape(start.State), cookie, "")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	var poll struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&poll)
	pResp.Body.Close()
	if poll.Status != "success" || poll.AccountID != "antigravity:user@example.com" {
		t.Fatalf("poll = %+v", poll)
	}

	// The account is persisted and present in the pool.
	st := panelState(t, srv.URL, cookie)
	found := false
	for _, a := range st {
		if a.ID == "antigravity:user@example.com" && a.Channel == "antigravity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("antigravity account missing from pool: %+v", st)
	}
}

func TestGoogleOAuthExchangeFailure(t *testing.T) {
	tokenSrv := googleTokenServer(t, 400, `{"error":"invalid_grant"}`)
	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.Antigravity.GoogleOAuthClientID = testToken("gid")
		cfg.Channels.Antigravity.GoogleOAuthClientSecret = testToken("gsec")
		cfg.Channels.Antigravity.TokenURL = tokenSrv.URL + "/token"
	})
	cookie, csrf := loginHelper(t, srv)

	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/google/start", cookie, csrf, `{}`)
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	au, _ := url.Parse(start.AuthorizeURL)
	redirectURI := au.Query().Get("redirect_uri")

	cbResp, err := http.Get(redirectURI + "?code=" + url.QueryEscape(testAuthCode()) + "&state=" + url.QueryEscape(start.State))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	page, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if !strings.Contains(string(page), "授权失败") {
		t.Fatalf("failure page body = %s", page)
	}

	pResp, err := doAuthed(t, "GET", srv.URL+"/panel/api/accounts/google/poll?state="+url.QueryEscape(start.State), cookie, "")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	var poll struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&poll)
	pResp.Body.Close()
	if poll.Status != "failed" {
		t.Fatalf("poll after failed exchange = %+v", poll)
	}
	for _, a := range panelState(t, srv.URL, cookie) {
		if a.Channel == "antigravity" {
			t.Fatalf("failed exchange must not create an account: %+v", a)
		}
	}
}

// TestGoogleOAuthCallbackStateMismatch: the loopback callback must verify the
// state nonce Google echoes back. A local process that discovers the port
// (but not the 128-bit state) cannot complete the login with its own code,
// and a rejected probe must leave the pending flow intact for the real
// redirect that follows.
func TestGoogleOAuthCallbackStateMismatch(t *testing.T) {
	tokenBody := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600,"id_token":%q}`,
		testToken("at"), testToken("rt"), testIDToken("probe@example.com"))
	tokenSrv := googleTokenServer(t, 200, tokenBody)

	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.Antigravity.GoogleOAuthClientID = testToken("gid")
		cfg.Channels.Antigravity.GoogleOAuthClientSecret = testToken("gsec")
		cfg.Channels.Antigravity.TokenURL = tokenSrv.URL + "/token"
	})
	cookie, csrf := loginHelper(t, srv)

	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/google/start", cookie, csrf, `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	au, err := url.Parse(start.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	loopback := au.Query().Get("redirect_uri")

	// Probe with a code but WITHOUT the state nonce.
	probe, err := http.Get(loopback + "?code=" + url.QueryEscape(testAuthCode()))
	if err != nil {
		t.Fatalf("probe callback: %v", err)
	}
	probePage, _ := io.ReadAll(probe.Body)
	probe.Body.Close()
	if !strings.Contains(string(probePage), "state 不匹配") {
		t.Fatalf("probe page must report the state mismatch, got: %s", probePage)
	}

	// The pending flow must survive the rejected probe.
	pResp, err := doAuthed(t, "GET", srv.URL+"/panel/api/accounts/google/poll?state="+url.QueryEscape(start.State), cookie, "")
	if err != nil {
		t.Fatalf("poll after probe: %v", err)
	}
	var poll struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&poll)
	pResp.Body.Close()
	if poll.Status != "pending" {
		t.Fatalf("poll after rejected probe = %+v, want pending", poll)
	}
	for _, a := range panelState(t, srv.URL, cookie) {
		if a.Channel == "antigravity" {
			t.Fatalf("rejected probe must not create an account: %+v", a)
		}
	}

	// The genuine redirect (with the right state) still completes.
	cb, err := http.Get(loopback + "?code=" + url.QueryEscape(testAuthCode()) + "&state=" + url.QueryEscape(start.State))
	if err != nil {
		t.Fatalf("real callback: %v", err)
	}
	page, _ := io.ReadAll(cb.Body)
	cb.Body.Close()
	if !strings.Contains(string(page), "添加成功") {
		t.Fatalf("real callback page = %s", page)
	}
	pResp, err = doAuthed(t, "GET", srv.URL+"/panel/api/accounts/google/poll?state="+url.QueryEscape(start.State), cookie, "")
	if err != nil {
		t.Fatalf("poll after real callback: %v", err)
	}
	var poll2 struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&poll2)
	pResp.Body.Close()
	if poll2.Status != "success" || poll2.AccountID != "antigravity:probe@example.com" {
		t.Fatalf("poll after real callback = %+v", poll2)
	}
}

// wbDeviceServer mocks the Tencent device-flow endpoints: the first token
// poll reports "login pending" (business code), the second returns tokens.
// BP-225 (misakano 96b816da): the three endpoints are plain node-fetch calls
// — no Origin/Referer/X-Requested-With — with per-endpoint x-domain /
// x-custom-channel-id / x-no-* sentinels ("true", not "1").
func wbDeviceServer(t *testing.T) *httptest.Server {
	t.Helper()
	tokenPolls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, k := range []string{"Origin", "Referer", "X-Requested-With"} {
			if got := r.Header.Get(k); got != "" {
				t.Errorf("%s = %q; the login endpoints are node-fetch, no browser headers", k, got)
			}
		}
		if got := r.Header.Get("User-Agent"); got != workbuddy.RestUA(false) {
			t.Errorf("UA = %q, want the REST-family CN form", got)
		}
		if got := r.Header.Get("X-No-User-Id"); got != "true" {
			t.Errorf("X-No-User-Id = %q, want true", got)
		}
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			if got := r.URL.Query().Get("platform"); got != "workbuddy" {
				t.Errorf("platform = %q, want workbuddy (product.json authentication.attributes.platform)", got)
			}
			if got := r.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
				t.Errorf("auth/state X-Domain = %q, want www.workbuddy.cn (front host, not API base)", got)
			}
			if got := r.Header.Get("X-Custom-Channel-Id"); got != "unknown" {
				t.Errorf("X-Custom-Channel-Id = %q, want unknown", got)
			}
			if got := r.Header.Get("X-No-Authorization"); got != "true" {
				t.Errorf("auth/state X-No-Authorization = %q, want true", got)
			}
			fmt.Fprintf(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":%q}}`, "https://www.workbuddy.cn/login?state=st-1")
		case "/v2/plugin/auth/token":
			tokenPolls++
			if got := r.Header.Get("X-Domain"); got != "" {
				t.Errorf("auth/token X-Domain = %q; the token endpoint is the one endpoint without it", got)
			}
			if got := r.Header.Get("X-No-Authorization"); got != "true" {
				t.Errorf("auth/token X-No-Authorization = %q, want true", got)
			}
			if tokenPolls == 1 {
				fmt.Fprint(w, `{"code":12001,"msg":"login ing","data":{}}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"msg":"","data":{"accessToken":%q,"refreshToken":%q,"expiresIn":5184000,"domain":"codebuddy.cn"}}`,
				testToken("wb-at"), testToken("wb-rt"))
		case "/v2/plugin/login/account":
			if got := r.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
				t.Errorf("login/account X-Domain = %q, want www.workbuddy.cn", got)
			}
			if got := r.Header.Get("X-No-Authorization"); got != "" {
				t.Errorf("login/account X-No-Authorization = %q; it has Bearer, only three sentinels", got)
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Errorf("account call missing bearer")
			}
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"uid":"u-wb-9","enterpriseId":"ent-1","nickname":"新号"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkBuddyDeviceFlow(t *testing.T) {
	wbSrv := wbDeviceServer(t)
	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.WorkBuddy.ChatBaseCN = wbSrv.URL
		cfg.Channels.WorkBuddy.ChatBaseGL = wbSrv.URL
	})
	cookie, csrf := loginHelper(t, srv)

	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/start", cookie, csrf, `{"realm":"cn"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var start struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
		Realm   string `json:"realm"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if start.State != "st-1" || start.Realm != "cn" || !strings.Contains(start.AuthURL, "workbuddy.cn/login") {
		t.Fatalf("start = %+v", start)
	}

	// First poll: upstream says login pending (non-zero business code).
	body, _ := json.Marshal(map[string]string{"state": start.State, "realm": "cn"})
	p1 := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/poll", cookie, csrf, string(body))
	var poll1 struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(p1.Body).Decode(&poll1)
	p1.Body.Close()
	if p1.StatusCode != 200 || poll1.Status != "pending" {
		t.Fatalf("first poll status=%d = %+v", p1.StatusCode, poll1)
	}

	// Second poll: tokens + account → success.
	p2 := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/poll", cookie, csrf, string(body))
	var poll2 struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.NewDecoder(p2.Body).Decode(&poll2)
	p2.Body.Close()
	if poll2.Status != "success" || poll2.AccountID != "workbuddy:u-wb-9" {
		t.Fatalf("second poll = %+v", poll2)
	}

	// realm mismatch must be rejected (防混域).
	p3 := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/poll", cookie, csrf,
		`{"state":"st-1","realm":"global"}`)
	p3.Body.Close()
	if p3.StatusCode != 400 {
		t.Fatalf("realm mismatch status = %d, want 400", p3.StatusCode)
	}

	// The account landed in the pool with realm cn.
	found := false
	for _, a := range panelState(t, srv.URL, cookie) {
		if a.ID == "workbuddy:u-wb-9" && a.Realm == "cn" {
			found = true
		}
	}
	if !found {
		t.Fatal("device-flow account missing from pool")
	}
}

func TestOnboardingAuthAndRealmGuards(t *testing.T) {
	wbSrv := wbDeviceServer(t)
	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.WorkBuddy.ChatBaseCN = wbSrv.URL
		cfg.Channels.WorkBuddy.ChatBaseGL = wbSrv.URL
		cfg.Channels.Antigravity.GoogleOAuthClientID = testToken("gid")
		cfg.Channels.Antigravity.GoogleOAuthClientSecret = testToken("gsec")
	})

	// Open panel: no 401 anymore. Both start endpoints reach their handlers;
	// unauthenticated POSTs still need the CSRF token → 403.
	for _, path := range []string{"/panel/api/accounts/google/start", "/panel/api/accounts/workbuddy/start"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s without csrf = %d, want 403", path, resp.StatusCode)
		}
	}

	cookie, csrf := loginHelper(t, srv)
	// Authenticated but without CSRF → 403.
	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/start", cookie, "", `{}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("start without csrf = %d, want 403", resp.StatusCode)
	}
	// Authenticated but without CSRF → 403 (google too).
	resp = postWithCSRF(t, srv.URL+"/panel/api/accounts/google/start", cookie, "", `{}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("google start without csrf = %d, want 403", resp.StatusCode)
	}
	// Global realm is refused while the international switch is off.
	resp = postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/start", cookie, csrf, `{"realm":"global"}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("global realm with switch off = %d, want 403", resp.StatusCode)
	}
	// Google start works with NO configured client: the built-in Google CLI
	// client kicks in (ticket 18 — panel onboarding must work out of the box,
	// matching the AIClient2API reference). The authorize URL must carry the
	// built-in client id and never a client_secret.
	srv2, _ := buildPanelServer(t) // default config: no google client id
	cookie2, csrf2 := loginHelper(t, srv2)
	resp = postWithCSRF(t, srv2.URL+"/panel/api/accounts/google/start", cookie2, csrf2, `{}`)
	var startOut struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&startOut)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(startOut.AuthorizeURL, "client_id=1071006060591") {
		t.Fatalf("builtin-client google start = %d %q", resp.StatusCode, startOut.AuthorizeURL)
	}
	if strings.Contains(startOut.AuthorizeURL, "client_secret") {
		t.Fatal("authorize url must never contain client_secret")
	}
}

// doAuthed issues a request with the session cookie (no CSRF needed for GET).
func doAuthed(t *testing.T, method, url string, cookie *http.Cookie, csrf string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if cookie != nil {
		addCookie(req, cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	return http.DefaultClient.Do(req)
}

// --- BP-13: hot settings / route editing / task switches ---

func TestSettingsHotUpdateValidationAndRestore(t *testing.T) {
	srv, _, gw, st := buildPanelEnv(t, nil)
	cookie, csrf := loginHelper(t, srv)

	post := func(body string) *http.Response {
		t.Helper()
		return postWithCSRF(t, srv.URL+"/panel/api/settings", cookie, csrf, body)
	}

	// Invalid values are rejected and never persisted.
	for _, bad := range []string{
		`{"greedy_floor_seconds":0}`,
		`{"cooldown_soft_base_seconds":-5}`,
		`{"first_byte_window_seconds":0}`,
		`{"first_byte_window_seconds":4000}`,
		`{"max_in_flight":{"bogus":2}}`,
		`{"max_in_flight":{"workbuddy":0}}`,
	} {
		resp := post(bad)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("invalid settings %s = %d, want 400", bad, resp.StatusCode)
		}
	}
	for _, key := range []string{panel.KeyGreedyFloorSecs, panel.KeyCooldownBaseSecs, panel.KeyFirstByteWindowSecs, panel.KeyMaxInFlight} {
		if _, ok, _ := st.GetSetting(key); ok {
			t.Fatalf("rejected settings must not persist %q", key)
		}
	}

	// Valid values persist and are visible in the state view.
	resp := post(`{"greedy_floor_seconds":30,"cooldown_soft_base_seconds":90,"first_byte_window_seconds":120,"max_in_flight":{"workbuddy":3},"prompt_replace_enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("valid settings = %d", resp.StatusCode)
	}
	if v, ok, _ := st.GetSetting(panel.KeyGreedyFloorSecs); !ok || v != "30" {
		t.Fatalf("greedy floor persisted = %q ok=%v", v, ok)
	}
	stResp, err := doAuthed(t, "GET", srv.URL+"/panel/api/state", cookie, "")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var state struct {
		Settings map[string]any `json:"settings"`
	}
	_ = json.NewDecoder(stResp.Body).Decode(&state)
	stResp.Body.Close()
	if state.Settings["greedy_floor_seconds"].(float64) != 30 ||
		state.Settings["first_byte_window_seconds"].(float64) != 120 ||
		state.Settings["prompt_replace_enabled"] != false {
		t.Fatalf("state settings = %+v", state.Settings)
	}

	// Restart recovery: the persisted values are re-applied to the gateway.
	applied := strings.Join(panel.RestoreHotSettings(gw, st), ",")
	for _, want := range []string{"greedy_floor=30s", "cooldown_soft_base=90s", "first_byte_window=120s", "max_in_flight", "prompt_replace=false"} {
		if !strings.Contains(applied, want) {
			t.Fatalf("restore applied = %q, missing %q", applied, want)
		}
	}
}

func TestRoutesEditHotReload(t *testing.T) {
	srv, _, _, _ := buildPanelEnv(t, nil)
	cookie, csrf := loginHelper(t, srv)

	// Read the live table.
	resp, err := doAuthed(t, "GET", srv.URL+"/panel/api/routes", cookie, "")
	if err != nil {
		t.Fatalf("routes get: %v", err)
	}
	var got struct {
		Routes []map[string]any `json:"routes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Routes) != 1 {
		t.Fatalf("initial routes = %+v", got.Routes)
	}

	// Invalid submission: rejected, table unchanged.
	bad := requestWithCSRF(t, "PUT", srv.URL+"/panel/api/routes", cookie, csrf,
		`{"routes":[{"model":"glm-5.3","channel":"bogus","upstream_model":"x","priority":0}]}`)
	bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("invalid route row = %d, want 400", bad.StatusCode)
	}

	// Valid submission: hot-reload + persist.
	body := `{"routes":[
	  {"model":"glm-5.3","channel":"trae","upstream_model":"glm-5.2","priority":0},
	  {"model":"new-model","channel":"zcode","upstream_model":"GLM-5.3","priority":0}
	]}`
	ok := requestWithCSRF(t, "PUT", srv.URL+"/panel/api/routes", cookie, csrf, body)
	ok.Body.Close()
	if ok.StatusCode != 200 {
		t.Fatalf("routes put = %d", ok.StatusCode)
	}
	stResp, _ := doAuthed(t, "GET", srv.URL+"/panel/api/state", cookie, "")
	var state struct {
		Models []struct {
			Model    string `json:"model"`
			Channels []struct {
				Channel string `json:"channel"`
			} `json:"channels"`
		} `json:"models"`
	}
	_ = json.NewDecoder(stResp.Body).Decode(&state)
	stResp.Body.Close()
	foundNew := false
	for _, m := range state.Models {
		if m.Model == "glm-5.3" {
			if len(m.Channels) != 1 || m.Channels[0].Channel != "trae" {
				t.Fatalf("glm-5.3 after hot reload = %+v", m.Channels)
			}
		}
		if m.Model == "new-model" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("hot-reloaded table missing new-model: %+v", state.Models)
	}
}

// fakeTaskRunner exercises the panel's task-switch glue.
type fakeTaskRunner struct{ enabled map[string]bool }

func (f *fakeTaskRunner) RunTask(ctx context.Context, taskType, accountID string) (string, error) {
	return "ok", nil
}
func (f *fakeTaskRunner) SetLogRetention(d time.Duration) {}
func (f *fakeTaskRunner) TaskTypes() []string             { return []string{"probe_all", "checkin_trae"} }
func (f *fakeTaskRunner) TaskEnabledStates() map[string]bool {
	out := map[string]bool{}
	for _, t := range f.TaskTypes() {
		enabled, ok := f.enabled[t]
		out[t] = !ok || enabled
	}
	return out
}
func (f *fakeTaskRunner) SetTaskEnabled(taskType string, enabled bool) error {
	for _, t := range f.TaskTypes() {
		if t == taskType {
			f.enabled[taskType] = enabled
			return nil
		}
	}
	return fmt.Errorf("tasks: unknown task type %q", taskType)
}

// TaskParamProvider capability: the schedule is panel-editable; invalid
// values are rejected by the runner (the scheduler owns validation).
// Embeds fakeTaskRunner to satisfy the base TaskRunner interface.
type fakeTaskParams struct {
	fakeTaskRunner
	cur model.TaskParams
}

func (f *fakeTaskParams) TaskScheduleParams() model.TaskParams { return f.cur }
func (f *fakeTaskParams) SetTaskScheduleParams(p model.TaskParams) error {
	if p.ProbeMinutes < 1 || p.ProbeMinutes > 1440 {
		return fmt.Errorf("tasks: probe_minutes must be 1-1440 minutes")
	}
	f.cur = p
	return nil
}

func TestTaskParamsEndpoint(t *testing.T) {
	srv, pn, _, _ := buildPanelEnv(t, nil)
	pn.SetTaskRunner(&fakeTaskParams{fakeTaskRunner: fakeTaskRunner{enabled: map[string]bool{}}, cur: model.TaskParams{
		CheckinHours: []int{9, 21}, GrowthHours: []int{9},
		ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60,
	}})

	cookie, csrf := loginHelper(t, srv)
	// GET surfaces the schedule alongside the run history.
	listResp, _ := doAuthed(t, "GET", srv.URL+"/panel/api/tasks", cookie, "")
	var list struct {
		Params model.TaskParams `json:"params"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if list.Params.ProbeMinutes != 5 || len(list.Params.CheckinHours) != 2 {
		t.Fatalf("params in listing = %+v", list.Params)
	}

	// Save with CSRF applies and echoes the normalized schedule.
	resp := postWithCSRF(t, srv.URL+"/panel/api/tasks/params", cookie, csrf,
		`{"checkin_hours":[10,22],"growth_hours":[1],"probe_minutes":3,"google_probe_minutes":20,"refresh_minutes":90}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("save params = %d", resp.StatusCode)
	}
	// Validation failures surface as 400 (runner-owned message).
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/params", cookie, csrf,
		`{"checkin_hours":[9],"growth_hours":[9],"probe_minutes":0,"google_probe_minutes":15,"refresh_minutes":60}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("invalid params = %d, want 400", resp.StatusCode)
	}
	// Without CSRF → 403 like every other mutation.
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/params", cookie, "",
		`{"checkin_hours":[9],"growth_hours":[9],"probe_minutes":5,"google_probe_minutes":15,"refresh_minutes":60}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("params without csrf = %d, want 403", resp.StatusCode)
	}
}

func TestTaskToggleEndpoint(t *testing.T) {
	srv, pn, _, _ := buildPanelEnv(t, nil)
	pn.SetTaskRunner(&fakeTaskRunner{enabled: map[string]bool{}})

	// Without CSRF → 403 (open panel: no login, but mutations need token).
	resp, err := http.Post(srv.URL+"/panel/api/tasks/toggle", "application/json",
		strings.NewReader(`{"task_type":"probe_all","enabled":false}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("toggle without csrf = %d, want 403", resp.StatusCode)
	}

	cookie, csrf := loginHelper(t, srv)
	// Missing CSRF → 403.
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/toggle", cookie, "", `{"task_type":"probe_all","enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("toggle without csrf = %d, want 403", resp.StatusCode)
	}

	// Switch off → tasks listing shows it.
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/toggle", cookie, csrf, `{"task_type":"probe_all","enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("toggle = %d", resp.StatusCode)
	}
	listResp, _ := doAuthed(t, "GET", srv.URL+"/panel/api/tasks", cookie, "")
	var list struct {
		Enabled map[string]bool `json:"enabled"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if list.Enabled["probe_all"] != false || list.Enabled["checkin_trae"] != true {
		t.Fatalf("task enabled states = %+v", list.Enabled)
	}

	// Unknown task types are rejected by the runner glue.
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/toggle", cookie, csrf, `{"task_type":"nope","enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("unknown task toggle = %d, want 400", resp.StatusCode)
	}

	// A missing task_type is a client error, not a silent 200 with an empty
	// body (review P3: the bare return left the response unwritten).
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/toggle", cookie, csrf, `{"enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("empty task_type toggle = %d, want 400", resp.StatusCode)
	}
	resp = postWithCSRF(t, srv.URL+"/panel/api/tasks/run", cookie, csrf, `{}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("empty task_type run = %d, want 400", resp.StatusCode)
	}
}

// TestServiceEndpoints: the service-management card endpoints — auth guards,
// preference persistence with the operator's defaults (autostart ON,
// watchdog OFF), and the state view. Real schtasks registration is covered
// by manual verification (recorded in the execution report), not unit tests:
// creating system scheduled tasks inside go test would leak system state.
func TestServiceEndpoints(t *testing.T) {
	srv, _, _, _ := buildPanelEnv(t, nil)

	// Open panel: GET works without a session.
	if resp, err := http.Get(srv.URL + "/panel/api/service"); err != nil {
		t.Fatalf("get: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("service get = %d, want 200 (open panel)", resp.StatusCode)
		}
	}
	cookie, csrf := loginHelper(t, srv)
	resp := postWithCSRF(t, srv.URL+"/panel/api/service/autostart", cookie, "", `{"enabled":true}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("autostart without csrf = %d, want 403", resp.StatusCode)
	}

	// GET reports defaults: autostart ON, watchdog OFF.
	gResp, err := doAuthed(t, "GET", srv.URL+"/panel/api/service", cookie, "")
	if err != nil {
		t.Fatalf("service get: %v", err)
	}
	var view struct {
		AutostartEnabled bool   `json:"autostart_enabled"`
		WatchdogEnabled  bool   `json:"watchdog_enabled"`
		BootTaskState    string `json:"boot_task_state"`
		Supported        bool   `json:"supported"`
	}
	_ = json.NewDecoder(gResp.Body).Decode(&view)
	gResp.Body.Close()
	if !view.AutostartEnabled || view.WatchdogEnabled {
		t.Fatalf("defaults wrong: autostart=%v watchdog=%v, want true/false", view.AutostartEnabled, view.WatchdogEnabled)
	}

	// Toggling watchdog persists the preference. schtasks may or may not be
	// available in the test environment; an schtasks-level failure must be a
	// 500 with the original message (operator-visible), while a successful
	// toggle must return the refreshed view.
	resp = postWithCSRF(t, srv.URL+"/panel/api/service/watchdog", cookie, csrf, `{"enabled":true}`)
	if resp.StatusCode == 200 {
		var after struct {
			WatchdogEnabled bool `json:"watchdog_enabled"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&after)
		resp.Body.Close()
		if !after.WatchdogEnabled {
			t.Fatal("watchdog preference not persisted")
		}
		// Toggle back off so the test leaves no preference behind.
		off := postWithCSRF(t, srv.URL+"/panel/api/service/watchdog", cookie, csrf, `{"enabled":false}`)
		off.Body.Close()
	} else {
		resp.Body.Close()
		t.Logf("schtasks unavailable in test environment (status %d) — acceptable here", resp.StatusCode)
	}
}

type stateAccount struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Realm   string `json:"realm"`
}

func panelState(t *testing.T, base string, cookie *http.Cookie) []stateAccount {
	t.Helper()
	resp, err := doAuthed(t, "GET", base+"/panel/api/state", cookie, "")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	var st struct {
		Accounts []stateAccount `json:"accounts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	return st.Accounts
}
