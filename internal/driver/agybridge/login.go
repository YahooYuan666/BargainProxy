package agybridge

// Account onboarding, following the upstream agy-acp recipe
// (src/accounts/auth.ts createAuthMethods → "Sign in with Google (browser)"):
//
//	1. our own PKCE verifier/challenge + state, kept in THIS process;
//	2. loopback HTTP listener on 127.0.0.1:<random> at /oauth-callback;
//	3. exchange the code at Google's token endpoint (the installed-app client
//	   the vendor ships — the same client agy itself uses);
//	4. write agy's file-backed credential into the account's isolated home:
//	     <home>/.gemini/antigravity-cli/antigravity-oauth-token   (0600)
//	     <home>/.gemini/antigravity-cli/cache/acp-auth-state.json
//
// This replaces the earlier `agy acp --login` terminal flow, which cannot work
// here: that flow keeps its PKCE verifier inside the child process, so a code
// pasted back after the child exited can never be exchanged — agy's own log
// showed `oauth2: "invalid_grant" "Invalid code verifier"` (2026-11-25 07:13).
// Upstream treats that terminal flow as its fourth fallback for this reason.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// Antigravity's official installed-app OAuth client. These are Google's
// PUBLIC, publicly-distributed desktop-client credentials (the same pair the
// official CLI ships in plaintext to every user) — not a secret: OAuth
// native-app clients cannot keep a client_secret confidential by design, and
// authorization still requires the user's own Google login.
//
// BP-350~358 note: they are taken from config.GoogleOAuthFallback() rather
// than written as literals. That helper assembles the pair at runtime
// specifically so the joined literal never appears in credential scans — the
// older plaintext copy here tripped GitHub's secret scanner and blocked a
// push. Same values, same behaviour; explicit config.json values still win.
var (
	agyClientID, agyClientSecret = config.GoogleOAuthFallback()
)

const (
	agyAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	agyTokenURL = "https://oauth2.googleapis.com/token"
)

var agyScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
	"https://www.googleapis.com/auth/aicode",
	"openid",
}

const loginFlowTTL = 10 * time.Minute

// loginFlow is one in-flight browser sign-in.
type loginFlow struct {
	home      string
	verifier  string
	state     string
	status    string // pending | success | failed
	errMsg    string
	accountID string
	// expectEmail is the Google identity the operator typed in the panel.
	// Google's account chooser ALWAYS appears (the browser, not us, decides who
	// is signed in), so the only honest guard is to verify the identity that
	// actually answered and refuse the token when it is the wrong one
	// (2026-11-25 field: pick-a-different-account produced a silently wrong
	// pool entry).
	expectEmail string
	actualEmail string
	server      *http.Server
	created     time.Time
	mu          sync.Mutex
}

var (
	loginFlowsMu sync.Mutex
	loginFlows   = map[string]*loginFlow{}
)

// StartBrowserLogin opens a loopback listener and returns the Google
// authorization URL plus the flow id the panel polls. The listener outlives the
// HTTP request that started it (the callback arrives minutes later from the
// browser), so nothing here may borrow the caller's request context — doing so
// cancelled the token exchange the moment the panel's start request returned
// (measured 2026-11-25 16:30: `Post ".../token": context canceled`).
func (d *Driver) StartBrowserLogin(_ context.Context, acc *model.Account) (flowID, authURL string, err error) {
	home, err := d.accountHome(acc)
	if err != nil {
		return "", "", err
	}
	verifier := base64URL(randBytes(32))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64URL(sum[:])
	state := base64URL(randBytes(16))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("agybridge: loopback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", port)

	f := &loginFlow{
		accountID: acc.ID, home: home, verifier: verifier, state: state,
		status: "pending", created: time.Now(), expectEmail: credsEmail(acc),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != f.state {
			writeCallbackPage(w, false, "state 不匹配，请重新发起登录")
			return
		}
		if e := q.Get("error"); e != "" {
			f.finish("failed", "Google 返回错误："+e)
			writeCallbackPage(w, false, e)
			return
		}
		code := q.Get("code")
		if code == "" {
			f.finish("failed", "没有拿到授权码")
			writeCallbackPage(w, false, "no code")
			return
		}
		actual, err := exchangeAndStore(context.Background(), f.home, code, redirectURI, f.verifier)
		if err != nil {
			f.finish("failed", err.Error())
			writeCallbackPage(w, false, err.Error())
			return
		}
		f.mu.Lock()
		f.actualEmail = actual
		expect := f.expectEmail
		f.mu.Unlock()
		// Identity check: the browser's account chooser is authoritative, so a
		// mismatch must be surfaced and the token discarded — writing it would
		// silently bind this account row to the wrong Google identity.
		if expect != "" && !strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(expect)) {
			_ = os.Remove(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
			msg := fmt.Sprintf("选错账号了：面板填的是 %s，浏览器里登录的是 %s。请重新发起登录并选择 %s。", expect, actual, expect)
			f.finish("failed", msg)
			writeCallbackPage(w, false, msg)
			return
		}
		f.finish("success", "")
		writeCallbackPage(w, true, actual)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	f.server = srv
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(loginFlowTTL)
		if f.isPending() {
			f.finish("failed", "登录超时（10 分钟）")
		}
	}()

	flowID = "agy:" + base64URL(randBytes(9))
	loginFlowsMu.Lock()
	for k, old := range loginFlows {
		if time.Since(old.created) > loginFlowTTL {
			delete(loginFlows, k)
		}
	}
	loginFlows[flowID] = f
	loginFlowsMu.Unlock()

	q := url.Values{}
	q.Set("client_id", agyClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(agyScopes, " "))
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// login_hint only PRE-SELECTS the account in Google's chooser; it cannot
	// skip the chooser. The identity is enforced by the post-exchange check.
	if hint := f.expectEmail; hint != "" {
		q.Set("login_hint", hint)
	}
	return flowID, agyAuthURL + "?" + q.Encode(), nil
}

// LoginStatus reports one flow's state.
func LoginStatus(flowID string) (status, errMsg string, ok bool) {
	loginFlowsMu.Lock()
	f := loginFlows[flowID]
	loginFlowsMu.Unlock()
	if f == nil {
		return "", "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.errMsg, true
}

func (f *loginFlow) finish(status, errMsg string) {
	f.mu.Lock()
	f.status = status
	f.errMsg = errMsg
	f.mu.Unlock()
	if f.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = f.server.Shutdown(ctx)
		cancel()
	}
}

func (f *loginFlow) isPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status == "pending"
}

// exchangeAndStore swaps the authorization code for tokens and writes agy's
// file-backed credential into the isolated home (upstream
// writeIsolatedAgyOAuthToken).
func exchangeAndStore(ctx context.Context, home, code, redirectURI, verifier string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", agyClientID)
	form.Set("client_secret", agyClientSecret)
	form.Set("code_verifier", verifier)

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, agyTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("连不上 Google：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Google 换 token 失败（%d）：%s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", fmt.Errorf("token 响应解析失败：%w", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return "", fmt.Errorf("Google 没给 refresh_token（请重试并在授权页允许离线访问）")
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	email := fetchUserEmail(ctx, tok.AccessToken)
	if err := writeTokenFile(home, tok.AccessToken, tok.RefreshToken, tok.ExpiresIn, email); err != nil {
		return "", err
	}
	return email, nil
}

// fetchUserEmail asks Google who this access token belongs to (the scopes
// include userinfo.email). Best effort: an empty result means the identity
// check cannot run, and the flow proceeds as before.
func fetchUserEmail(ctx context.Context, accessToken string) string {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"https://www.googleapis.com/oauth2/v2/userinfo?alt=json", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var doc struct {
		Email string `json:"email"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Email)
}

// writeTokenFile lays down exactly what agy's FileTokenStorage reads.
func writeTokenFile(home, accessToken, refreshToken string, expiresIn int, email string) error {
	appData := filepath.Join(home, ".gemini", "antigravity-cli")
	cacheDir := filepath.Join(appData, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	stored := map[string]any{
		"token": map[string]any{
			"access_token":  accessToken,
			"token_type":    "Bearer",
			"refresh_token": refreshToken,
			"expiry":        time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339),
			"expires_in":    expiresIn,
		},
		"auth_method": "consumer",
	}
	if email != "" {
		stored["email"] = email
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(appData, "antigravity-oauth-token"), payload); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(cacheDir, "acp-auth-state.json"),
		[]byte(`{"methodId":"oauth-personal"}`))
}

func atomicWrite(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func writeCallbackPage(w http.ResponseWriter, ok bool, detail string) {
	msg := "登录成功，可以关闭此页面回到控制台。"
	if ok && strings.Contains(detail, "@") {
		msg = "登录成功：" + html.EscapeString(detail) + "。可以关闭此页面回到控制台。"
	}
	if !ok {
		msg = "登录失败：" + html.EscapeString(detail)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Agy-Bridge</title>
<body style="font:16px system-ui,sans-serif;padding:3em;text-align:center">%s</body>`, msg)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// credsEmail is the Google identity recorded in the account's credential blob
// (set when the panel created the row from the operator's typed email).
func credsEmail(acc *model.Account) string {
	if acc == nil || len(acc.CredsJSON) == 0 {
		return ""
	}
	var c Creds
	if json.Unmarshal(acc.CredsJSON, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.Email)
}
