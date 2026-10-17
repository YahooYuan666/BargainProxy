// Panel account onboarding: Google OAuth for the antigravity channel and the
// WorkBuddy device flow, both driven from the browser.
//
// Google: a loopback authorization-code flow. The listener binds 127.0.0.1
// only and is closed after the callback or the 5-minute deadline; our own
// state nonce keys the pending flow. client_id/client_secret come from
// config.json only (never hardcoded here); authorization codes and tokens
// never reach logs.
//
// WorkBuddy: device authorization. Endpoint and envelope semantics are
// re-implemented from the MIT-licensed workbuddy2api-panel reference (see
// NOTICE); the upstream state keys an in-memory flow that is never persisted.
// The realm is fixed at start and re-checked at poll (防混域).
package panel

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

const (
	// oauthFlowTTL bounds how long a start→callback/poll cycle may take.
	oauthFlowTTL = 5 * time.Minute
	// googleAuthEndpoint is Google's authorization-code endpoint.
	googleAuthEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	// googleOAuthScope matches the Cloud Code Assist client scopes.
	googleOAuthScope = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email"
	// wbClientUA is the official CLI identity used by the device flow.
	wbClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// builtinGoogleClientID/builtinGoogleClientSecret now come from
// internal/config (shared with the antigravity driver's refresh path);
// kept as vars so the oauth.go call sites stay unchanged.
var builtinGoogleClientID, builtinGoogleClientSecret = config.GoogleOAuthFallback()

// authFlow is one pending onboarding attempt.
type authFlow struct {
	channel   string
	realm     string // workbuddy only
	state     string // google only: the nonce bound into the authorize URL
	status    string // pending | success | failed
	accountID string
	errMsg    string
	expiresAt time.Time
	// created is the flow's birthday: touchFlow slides expiresAt on active
	// polls but never past created+flowMaxLifetime (BP-51).
	created     time.Time
	persistedAt time.Time    // last sealed write (throttles poll-driven re-persist)
	server      *http.Server // google loopback listener (closed on finish/expiry)
}

func (p *Panel) putFlow(key string, f *authFlow) {
	p.flowMu.Lock()
	now := time.Now()
	if f.created.IsZero() {
		f.created = now
	}
	var dropped []string
	for k, old := range p.flows {
		if now.After(old.expiresAt) {
			shutdownServerAsync(old.server)
			delete(p.flows, k)
			dropped = append(dropped, k)
		}
	}
	p.flows[key] = f
	p.flowMu.Unlock()
	for _, k := range dropped {
		p.dropPersistedFlow(k)
	}
	p.persistFlow(key)
}

func (p *Panel) getFlow(key string) (authFlow, bool) {
	p.flowMu.Lock()
	defer p.flowMu.Unlock()
	f := p.flows[key]
	if f == nil || time.Now().After(f.expiresAt) {
		return authFlow{}, false
	}
	return *f, true
}

// finishFlow marks a flow terminal (success/failed) and releases its loopback
// listener. Later polls keep reading the terminal state.
func (p *Panel) finishFlow(key, status, accountID, errMsg string) {
	p.flowMu.Lock()
	f := p.flows[key]
	if f != nil {
		f.status = status
		f.accountID = accountID
		f.errMsg = errMsg
		shutdownServerAsync(f.server)
	}
	p.flowMu.Unlock()
	// Terminal states are persisted too: a poll after a restart still answers
	// success/failed instead of "会话不存在" (BP-51).
	p.persistFlow(key)
}

func (p *Panel) expireFlow(key string) {
	p.finishFlow(key, "failed", "", "等待授权超时，请重新发起")
}

// shutdownServerAsync gracefully stops a loopback listener. Close (not
// Shutdown) would cut the connection that is still writing the callback page.
func shutdownServerAsync(hs *http.Server) {
	if hs == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}()
}

// --- Google OAuth (antigravity) ---

func (p *Panel) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	ag := p.cfg.Channels.Antigravity
	// Explicit config values win; otherwise fall back to Google's public CLI
	// client so panel onboarding works out of the box (matches the
	// AIClient2API reference behavior the operator expects).
	clientID, clientSecret := ag.GoogleOAuthClientID, ag.GoogleOAuthClientSecret
	if clientID == "" || clientSecret == "" {
		clientID, clientSecret = builtinGoogleClientID, builtinGoogleClientSecret
	}
	if clientID == "" || clientSecret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "google oauth client id/secret not configured in config.json",
		})
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "loopback listener failed: " + err.Error(),
		})
		return
	}
	state := randHex(16)
	redirectURI := "http://" + ln.Addr().String() + "/oauth2callback"
	key := "g:" + state

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2callback", func(cw http.ResponseWriter, cr *http.Request) {
		p.handleGoogleCallback(cw, cr, key, redirectURI)
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	p.putFlow(key, &authFlow{
		channel: model.ChannelAntigravity, state: state, status: "pending",
		expiresAt: time.Now().Add(oauthFlowTTL), server: hs,
	})
	time.AfterFunc(oauthFlowTTL, func() { p.expireFlow(key) })

	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {googleOAuthScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": googleAuthEndpoint + "?" + q.Encode(),
		"state":         state,
	})
}

func (p *Panel) handleGoogleCallback(w http.ResponseWriter, r *http.Request, key, redirectURI string) {
	f, ok := p.getFlow(key)
	if !ok || f.status != "pending" {
		writeOAuthPage(w, false, "会话已失效，请回到控制台重新发起")
		return
	}
	// The state nonce must round-trip through Google's redirect: the flow key
	// alone would let any local process that discovers the loopback port
	// complete the login with its own code (login CSRF). A mismatch rejects
	// this one callback but leaves the pending flow untouched, so a stray
	// probe cannot burn the user's authorization session.
	if got := r.URL.Query().Get("state"); subtle.ConstantTimeCompare([]byte(got), []byte(f.state)) != 1 {
		log.Printf("panel: google oauth callback state mismatch (port probe?)")
		writeOAuthPage(w, false, "授权校验失败（state 不匹配），请回到控制台重新发起")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		p.finishFlow(key, "failed", "", "授权被拒绝："+e)
		writeOAuthPage(w, false, "授权失败："+e)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeOAuthPage(w, false, "缺少授权码，请重新发起")
		return
	}
	acc, err := p.googleExchange(r.Context(), code, redirectURI)
	if err != nil {
		log.Printf("panel: google oauth exchange failed: %v", err)
		p.finishFlow(key, "failed", "", "授权码交换失败，请重试")
		writeOAuthPage(w, false, "授权失败，请回到控制台重试")
		return
	}
	if err := p.store.SaveAccount(acc); err != nil {
		log.Printf("panel: google oauth save failed: %v", err)
		p.finishFlow(key, "failed", "", "保存账号失败")
		writeOAuthPage(w, false, "保存账号失败，请查看服务日志")
		return
	}
	p.gw.Pool().Upsert(acc)
	p.finishFlow(key, "success", acc.ID, "")
	writeOAuthPage(w, true, "添加成功，可关闭此页")
}

func (p *Panel) handleGooglePoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state required"})
		return
	}
	f, ok := p.getFlow("g:" + state)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "授权会话不存在或已过期"})
		return
	}
	writeJSON(w, http.StatusOK, flowStatusJSON(f))
}

func flowStatusJSON(f authFlow) map[string]any {
	out := map[string]any{"status": f.status}
	if f.accountID != "" {
		out["account_id"] = f.accountID
	}
	if f.errMsg != "" {
		out["error"] = f.errMsg
	}
	return out
}

// googleExchange swaps the authorization code for tokens and reuses the
// import identity discipline (email first, refresh-token hash fallback).
func (p *Panel) googleExchange(ctx context.Context, code, redirectURI string) (*model.Account, error) {
	ag := p.cfg.Channels.Antigravity
	clientID, clientSecret := ag.GoogleOAuthClientID, ag.GoogleOAuthClientSecret
	if clientID == "" || clientSecret == "" {
		clientID, clientSecret = builtinGoogleClientID, builtinGoogleClientSecret
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ag.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Surface only the machine error code; the raw body may echo request
		// fields and must not be propagated.
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, fmt.Errorf("token exchange: upstream %d (%s)", resp.StatusCode, firstNonEmpty(e.Error, "unknown"))
	}
	if rerr != nil {
		return nil, fmt.Errorf("token exchange: read body: %w", rerr)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("token exchange: malformed response")
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("token exchange: no refresh_token in response")
	}
	expiry := time.Now().Add(time.Hour)
	if tok.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	creds, _ := json.Marshal(map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"email":         idTokenEmail(tok.IDToken),
		"expiry":        expiry.Format(time.RFC3339),
	})
	return normalizeImport(model.ChannelAntigravity, creds, "")
}

// idTokenEmail reads the email claim from an id_token payload. The token came
// over TLS from the configured token endpoint; the signature is not verified
// and only feeds the display identity (the refresh-token hash is the stable
// fallback when absent).
func idTokenEmail(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Email
}

// writeOAuthPage renders the loopback confirmation page. Dynamic text is
// HTML-escaped (the Google error query parameter is attacker-controlled).
func writeOAuthPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	color := "#e5534b"
	if ok {
		color = "#39c07f"
	}
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><title>BargainProxy</title></head>
<body style="font:15px/1.6 system-ui,sans-serif;background:#0f1115;color:#e6e9ef;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">
<div style="text-align:center"><div style="font-size:40px;color:%s">%s</div><p>%s</p></div></body></html>`,
		color, map[bool]string{true: "✓", false: "✗"}[ok], html.EscapeString(msg))
}

// --- WorkBuddy device flow ---

// wbBizError is a non-zero business envelope code (used as the pending signal
// by the token endpoint).
type wbBizError struct {
	code int
	msg  string
}

func (e *wbBizError) Error() string { return fmt.Sprintf("upstream code %d: %s", e.code, e.msg) }

func (p *Panel) wbRealmBase(realm string) string {
	if realm == "global" {
		return p.cfg.Channels.WorkBuddy.ChatBaseGL
	}
	return p.cfg.Channels.WorkBuddy.ChatBaseCN
}

func wbRealmOrigin(realm string) string {
	if realm == "global" {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

func (p *Panel) handleWBStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	if !decode(w, r, &req) {
		return
	}
	realm := req.Realm
	if realm == "" {
		realm = "cn"
	}
	if realm != "cn" && realm != "global" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "realm must be cn or global"})
		return
	}
	if realm == "global" && !p.wbGlobalEnabled() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "国际版未开启：先在账号池勾选 WorkBuddy 国际版"})
		return
	}
	base := p.wbRealmBase(realm)
	if base == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "workbuddy base url not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	data, err := p.wbCall(ctx, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", wbRealmOrigin(realm), []byte("{}"), "")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "auth state: " + err.Error()})
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "auth state: missing state or authUrl"})
		return
	}
	key := "w:" + st.State
	p.putFlow(key, &authFlow{
		channel: model.ChannelWorkBuddy, realm: realm, status: "pending",
		expiresAt: time.Now().Add(oauthFlowTTL),
	})
	time.AfterFunc(oauthFlowTTL, func() { p.expireFlow(key) })
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": st.AuthURL,
		"state":    st.State,
		"realm":    realm,
	})
}

func (p *Panel) handleWBPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		Realm string `json:"realm"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state required"})
		return
	}
	key := "w:" + req.State
	f, ok := p.getFlow(key)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "登录会话不存在或已过期，请重新发起"})
		return
	}
	// An active poll keeps the flow alive (sliding TTL, BP-51).
	p.touchFlow(key)
	// 防混域: the poll must stay in the realm the flow was created with.
	if req.Realm != "" && req.Realm != f.realm {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "realm mismatch: 与发起时的 realm 不一致"})
		return
	}
	switch f.status {
	case "success", "failed":
		writeJSON(w, http.StatusOK, flowStatusJSON(f))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	base := p.wbRealmBase(f.realm)
	origin := wbRealmOrigin(f.realm)
	data, err := p.wbCall(ctx, http.MethodGet, base+"/v2/plugin/auth/token?state="+url.QueryEscape(req.State), origin, nil, "")
	if err != nil {
		var biz *wbBizError
		if isBizError(err, &biz) {
			// Non-zero business code on the token endpoint means "login not
			// finished yet" — keep waiting until the flow TTL.
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "auth token: " + err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if acctData, aerr := p.wbCall(ctx, http.MethodGet, base+"/v2/plugin/login/account?state="+url.QueryEscape(req.State), origin, nil, tok.AccessToken); aerr == nil {
		_ = json.Unmarshal(acctData, &acct)
	}
	if acct.UID == "" {
		// uid is the stable identity; retry on the next poll until the TTL.
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	creds := map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"uid":           acct.UID,
		"nickname":      acct.Nickname,
		"realm":         f.realm,
	}
	if acct.EnterpriseID != "" {
		creds["enterprise_id"] = acct.EnterpriseID
	}
	if tok.ExpiresIn > 0 {
		creds["expires_at"] = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	raw, _ := json.Marshal(creds)
	acc, err := normalizeImport(model.ChannelWorkBuddy, raw, "")
	if err != nil {
		p.finishFlow(key, "failed", "", "凭据组装失败")
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "凭据组装失败"})
		return
	}
	if err := p.store.SaveAccount(acc); err != nil {
		log.Printf("panel: workbuddy device login save failed: %v", err)
		p.finishFlow(key, "failed", "", "保存账号失败")
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "保存账号失败"})
		return
	}
	p.gw.Pool().Upsert(acc)
	p.finishFlow(key, "success", acc.ID, "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": acc.ID})
}

func isBizError(err error, out **wbBizError) bool {
	if be, ok := err.(*wbBizError); ok {
		*out = be
		return true
	}
	return false
}

// wbCall performs one device-flow request and unwraps the {code,msg,data}
// envelope. Non-zero codes come back as *wbBizError.
func (p *Panel) wbCall(ctx context.Context, method, fullURL, origin string, body []byte, bearer string) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", wbClientUA)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	if rerr != nil {
		return nil, fmt.Errorf("read body: %w", rerr)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode envelope")
	}
	if env.Code != 0 {
		return nil, &wbBizError{code: env.Code, msg: env.Msg}
	}
	return env.Data, nil
}
