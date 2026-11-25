// Overseas AutoClaw sign-in (Google / z.ai), done entirely in this process.
//
// The overseas lane's sign-in is an OAuth web flow, and the reason this file
// exists is that "it needs a browser" is not the same as "it is closed". A
// comment here used to say a real-human attempt on 2026-09-20 came back 405
// behind an Alibaba edge block, and the import path was built around that
// conclusion. That reading was wrong for the interactive case: the official
// client (and magpie's autoclaw plugin, which reverse-engineered it) runs a
// local HTTP server, drives the whole sign-in inside it, and catches the
// provider's callback on the loopback port. No edge block is involved because
// the browser talks to us, not to the gateway, until the code is exchanged
// from the server side.
//
// The flow, as the official client performs it:
//
//	GET  /userapi/overseasv1/oauth-captcha-config     do we need the slider, and with what scene id
//	POST /userapi/overseasv1/{vendor}-oauth-url       -> the provider's authorize URL (and the state it minted)
//	[user approves in the browser; the provider redirects to our loopback]
//	POST /userapi/overseasv1/{vendor}-oauth-login     code + state -> access/refresh tokens
//
// Three constraints are load-bearing and each one was a failure first:
//
//  1. redirect_uri must be http://localhost:<port>/auth/callback-<vendor>,
//     with the port drawn from the official TokenServer pool below. Any other
//     port or path is refused with {"detail":"Redirect URI not registered for
//     this client"}.
//  2. The state the gateway put in the authorize URL must come back unchanged.
//     A callback without it is dropped — otherwise anyone could hand us their
//     own code and sign this installation into their account.
//  3. Every sign-in mints a NEW random device id. This is what makes more than
//     one account per machine possible: the account's device identity is the
//     value sent at sign-in and the server records it in the token, so reusing
//     one id across accounts makes the server treat them as one device (which
//     is also what zeroes out new-user entitlements). The CN SMS path derives
//     a id from the phone for a different reason (see DeviceIDForPhone); the
//     overseas path must not do that.
package autoclaw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/model"
)

// oauthClientVersion is the client version claimed on the sign-in endpoints.
//
// This is checked, and it moves: claiming 2.0.3 made the gateway answer
//   code=631002 "This version is no longer supported. Download the latest
//   version from our website."
// for both endpoints (field 2026-11-25, real overseas account). The installed
// official client here is 2.5.3.514, so the sign-in path claims 2.5.3 while the
// chat path keeps its own constant: they are different gates, and bumping one
// does not prove anything about the other.
//
// When this error comes back, read the installed client's version
// (AutoClaw2.exe → VersionInfo) and bump this, not the chat constant.
const oauthClientVersion = "2.5.3"

// oauthPorts is the official desktop client's own TokenServer callback pool.
//
// These belong to the CLIENT, not to us: on a machine where AutoClaw is
// installed and running, the client already holds several of them (field
// 2026-11-25: 18432 and 19723 held by AutoClaw/AutoClaw2, 19654 held by a
// leaking flow of ours). Trying them first is still worth it — when one is
// free it is the exact redirect target the OAuth client has registered — but
// it must not be the ONLY option, or sign-in is impossible whenever the client
// happens to be running, which is the common case.
var oauthPorts = []int{18432, 19654, 19723, 53699}

// oauthSessionTimeout bounds a sign-in that the operator abandons, so the
// loopback port is released instead of being held forever.
const oauthSessionTimeout = 10 * time.Minute

// newDeviceID mints a device id in the official client's shape: 32 random
// bytes, hex encoded (64 characters).
//
// Random on purpose. The account's device identity is whatever was sent at
// sign-in, so a fresh one per sign-in is what lets several accounts live on
// one machine without the server collapsing them into one device.
func newDeviceID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out), nil
}

// OAuthFlow is one in-flight overseas sign-in.
type OAuthFlow struct {
	// URL is the local guide page the operator opens. The slider (when the
	// gateway asks for one) is completed there, and the page then walks the
	// browser through the provider's authorize URL.
	URL string
	// Vendor is "google" or "zai".
	Vendor string

	deviceID  string
	d         *Driver
	state     string // the state the gateway minted; a callback must echo it
	code      string
	err       error
	mu        sync.Mutex
	server    *http.Server
	listener  net.Listener
	port      int
	timeout   time.Duration // 0 = oauthSessionTimeout
	done      chan struct{}
	closeOnce sync.Once
}

// oauthHeaders builds the header set the overseas sign-in endpoints accept.
//
// This is the official 2.0.3 desktop fingerprint, and it is NOT interchangeable
// with the generic userapi headers: sending those (X-Version 1.12.1, X-Tm web,
// X-Client-Type web) to /userapi/overseasv1/* answers 405 with an Alibaba edge
// block page, which reads like "this route is gone" but is really "this
// fingerprint is not the desktop client" (field 2026-11-25).
//
// The token, when present, rides a lower-case "authorization" header — the
// overseas lane's convention (the CN lane uses X-Authorization).
func oauthHeaders(token, secret string, withStainless bool) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("X-Version", oauthClientVersion)
	h.Set("X-Tm", platformTm())
	h.Set("X-Product", "autoclaw")
	h.Set("X-Client-Type", "pc")
	h.Set("X-Channel", overseasChannel)
	h.Set("X-Lang", overseasLang)
	h.Set("X-Auth-Appid", appID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", sign(ts, secret))
	h.Set("X-Trace-Id", traceID())
	if withStainless {
		// The desktop client's model calls go through OpenAI SDK 6.26.0; the
		// OAuth endpoints are called by the app itself, so they carry the
		// plain client shape rather than the SDK header family.
		h.Set("User-Agent", "AutoClaw/"+oauthClientVersion+" (win-x64)")
	}
	if t := strings.TrimSpace(token); t != "" {
		h.Set("authorization", t)
	}
	return h
}

// overseasCall posts to one /userapi/overseasv1/* endpoint with the desktop
// fingerprint and unwraps the {code,msg,data} envelope.
func (f *OAuthFlow) overseasCall(ctx context.Context, path string, body any, out any) error {
	secret := strings.TrimSpace(f.d.cfg.AppSecret)
	if secret == "" {
		return fmt.Errorf("autoclaw: channels.autoclaw.app_secret 未配置")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.d.overseasBase()+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header = oauthHeaders("", secret, true)
	resp, err := f.d.rpc.Do(req)
	if err != nil {
		return fmt.Errorf("autoclaw: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("autoclaw: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// A 405 here comes back as an HTML edge page, so the body is noise.
		// Report the status and a short prefix of whatever came back, and say
		// what it usually means.
		note := "（非 JSON 响应：多为指纹被边缘拦截，或该入口已下线）"
		if len(b) > 0 && b[0] == '{' {
			note = ""
		}
		return fmt.Errorf("autoclaw 登录接口 HTTP %d%s: %s", resp.StatusCode, note, shortenForError(string(b), 160))
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("autoclaw: decode envelope: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("autoclaw 登录接口 code=%d: %s", env.Code, env.Msg)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("autoclaw: decode data: %w", err)
		}
	}
	return nil
}

// shortenForError trims a response body down to something an error message can
// carry without pasting an HTML page into the panel.
func shortenForError(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// StartOAuth opens the loopback listener and returns the guide-page URL.
//
// The caller shows the URL to the operator and then calls Wait with a context;
// nothing blocks here, so a panel request can return immediately.
func (d *Driver) StartOAuth(ctx context.Context, vendor string) (*OAuthFlow, error) {
	v := strings.ToLower(strings.TrimSpace(vendor))
	// z.ai is the only sign-in the overseas gateway still serves. Google was
	// removed upstream — the entry point answers 405 with an edge block page,
	// verified against a real account 2026-11-25. Offering it would only send
	// the operator into a dead end, so it is refused here with a message that
	// says what to use instead.
	if v == "google" {
		return nil, fmt.Errorf("AutoClaw 海外版已不支持 Google 登录（上游 405）。请改用 z.ai 账号登录")
	}
	if v != "zai" {
		return nil, fmt.Errorf("autoclaw: unknown sign-in vendor %q (zai is the only one upstream still serves)", v)
	}
	deviceID, err := newDeviceID()
	if err != nil {
		return nil, fmt.Errorf("autoclaw: device id: %w", err)
	}
	ln, port, err := listenOAuthPort()
	if err != nil {
		return nil, fmt.Errorf("autoclaw: no sign-in port available: %w", err)
	}
	f := &OAuthFlow{
		Vendor:   v,
		d:        d,
		deviceID: deviceID,
		port:     port,
		done:     make(chan struct{}),
		URL:      fmt.Sprintf("http://127.0.0.1:%d/", port),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handleGuide)
	mux.HandleFunc("/captcha-config", f.handleCaptchaConfig)
	mux.HandleFunc("/open-auth", f.handleOpenAuth)
	mux.HandleFunc("/auth/callback-"+v, f.handleCallback)
	f.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := f.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.fail(err)
		}
	}()
	go func() {
		to := f.timeout
		if to == 0 {
			to = oauthSessionTimeout
		}
		select {
		case <-time.After(to):
			// Reclaim the port: nothing is coming, and holding the listener
			// would leave it bound in the gateway process for its whole
			// lifetime (field 2026-11-25: a flow that timed out this way kept
			// 19654 LISTENING until the gateway was restarted, and that port is
			// one of the client's own).
			f.fail(errors.New("sign-in timed out; start it again"))
			f.Close()
		case <-f.done:
		}
	}()
	return f, nil
}

// StartOAuthWithTimeout is StartOAuth with an injectable session timeout, so a
// test can exercise the real timeout path instead of calling Close by hand
// (hand-calling Close hides whether the timeout itself releases the port, which
// is exactly the leak that was shipped).
func (d *Driver) StartOAuthWithTimeout(ctx context.Context, vendor string, timeout time.Duration) (*OAuthFlow, error) {
	f, err := d.StartOAuth(ctx, vendor)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		f.timeout = timeout
	}
	return f, nil
}

// listenOAuthPort binds a loopback port for the sign-in page.
//
// It tries the official client's own ports first, because a port from that set
// is the exact redirect target the OAuth client has registered. On a machine
// with AutoClaw running, though, those ports are taken by the client itself,
// so a free ephemeral port is the fallback — and that is the common case, not
// an edge case (field 2026-11-25: all four were held, three by the client).
//
// The redirect_uri is sent to the gateway at authorize time, so the gateway
// validates whatever port we actually bound rather than a fixed one.
func listenOAuthPort() (net.Listener, int, error) {
	for _, p := range oauthPorts {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			return ln, p, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("no loopback port available: %w", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// redirectURI is the callback the gateway will redirect to. Hostname
// "localhost" is what the OAuth client expects; the port is whatever we
// actually bound and what we told the gateway.
func (f *OAuthFlow) redirectURI() string {
	return fmt.Sprintf("http://localhost:%d/auth/callback-%s", f.port, f.Vendor)
}

func (f *OAuthFlow) handleGuide(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, guidePageHTML(f.Vendor))
}

// handleCaptchaConfig reports whether the slider is required and with which
// scene id. A failure here is not fatal: the gateway re-checks the receipt
// itself, so an unreachable config just means "try without one".
func (f *OAuthFlow) handleCaptchaConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := f.captchaConfig(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"required": false})
		return
	}
	json.NewEncoder(w).Encode(cfg)
}

func (f *OAuthFlow) handleOpenAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		CaptchaReceipt string `json:"captcha_receipt"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)

	authURL, state, err := f.requestAuthorizeURL(r.Context(), body.CaptchaReceipt)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"url": authURL})
}

// handleCallback receives the provider's redirect. A code without the state we
// handed out is refused and the flow stays open, so a stray or forged callback
// cannot end the real one.
func (f *OAuthFlow) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")

	f.mu.Lock()
	want := f.state
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch {
	case code == "":
		io.WriteString(w, resultPageHTML(false, "没有收到授权码，请回引导页重试。"))
	case want == "":
		io.WriteString(w, resultPageHTML(false, "授权尚未发起或已结束，请回引导页重试。"))
	case state != want:
		// Refuse, but do not close: the legitimate callback may still arrive.
		io.WriteString(w, resultPageHTML(false, "回调校验失败（state 不匹配），已拒绝。"))
	default:
		f.mu.Lock()
		f.code = code
		f.mu.Unlock()
		io.WriteString(w, resultPageHTML(true, "授权成功，可以回到面板了。"))
		f.notify()
	}
}

// captchaConfig asks the gateway whether a slider is needed.
func (f *OAuthFlow) captchaConfig(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := f.overseasCall(ctx, "/userapi/overseasv1/oauth-captcha-config", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// requestAuthorizeURL asks for the provider's authorize URL and reads the state
// out of it. A URL without a state is refused: accepting it would make the
// callback check above vacuous.
func (f *OAuthFlow) requestAuthorizeURL(ctx context.Context, receipt string) (string, string, error) {
	body := map[string]any{
		"source_id":    "autoclaw",
		"device_id":    f.deviceID,
		"navigate_uri": f.redirectURI(),
	}
	if r := strings.TrimSpace(receipt); r != "" {
		body["ali_captcha_verify_param"] = r
	}
	var data struct {
		OAuthURL string `json:"oauth_url"`
		URL      string `json:"url"`
		LoginURL string `json:"login_url"`
		AuthURL  string `json:"auth_url"`
		Verify   string `json:"verify_url"`
	}
	path := fmt.Sprintf("/userapi/overseasv1/%s-oauth-url", f.Vendor)
	if err := f.overseasCall(ctx, path, body, &data); err != nil {
		return "", "", err
	}
	authURL := firstNonEmpty(data.OAuthURL, data.URL, data.LoginURL, data.AuthURL, data.Verify)
	if authURL == "" {
		return "", "", errors.New("the gateway returned no authorize URL")
	}
	state := stateFromURL(authURL)
	if state == "" {
		return "", "", errors.New("the authorize URL carries no state; refusing to continue")
	}
	return authURL, state, nil
}

// stateFromURL pulls ?state= out of an authorize URL without pulling in a URL
// parser dependency at the call site.
func stateFromURL(raw string) string {
	i := strings.Index(raw, "?")
	if i < 0 {
		return ""
	}
	for _, pair := range strings.Split(raw[i+1:], "&") {
		k, v, ok := strings.Cut(pair, "=")
		if ok && k == "state" {
			return v
		}
	}
	return ""
}

// Wait blocks until the callback arrives, the flow fails, or ctx ends, then
// exchanges the code for tokens and returns the account.
// ErrOAuthPending means the operator has not finished in the browser yet. It
// is returned instead of an error because a poll timing out says nothing
// about the sign-in — and it must never tear the flow down: the panel polls
// every couple of seconds, so closing on a poll timeout would kill the listener
// before the operator had time to approve anything (this was a real bug: the
// second poll killed the flow, and the page then reported "Failed to fetch").
var ErrOAuthPending = errors.New("sign-in still pending")

func (f *OAuthFlow) Wait(ctx context.Context) (*model.Account, error) {
	select {
	case <-f.done:
	case <-ctx.Done():
		// Do NOT close: this is a poll timing out, not the operator giving up.
		// The flow's own oauthSessionTimeout is what eventually reclaims it.
		return nil, ErrOAuthPending
	}
	f.mu.Lock()
	code, ferr := f.code, f.err
	f.mu.Unlock()
	if ferr != nil {
		f.Close()
		return nil, ferr
	}
	if code == "" {
		f.Close()
		return nil, errors.New("sign-in ended without an authorization code")
	}
	acct, err := f.exchange(ctx, code)
	f.Close()
	return acct, err
}

// exchange trades the code for tokens and builds the account row.
func (f *OAuthFlow) exchange(ctx context.Context, code string) (*model.Account, error) {
	f.mu.Lock()
	state := f.state
	f.mu.Unlock()
	body := map[string]any{
		"source_id":    "autoclaw",
		"device_id":    f.deviceID,
		"code":         code,
		"state":        state,
		"navigate_uri": f.redirectURI(),
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	path := fmt.Sprintf("/userapi/overseasv1/%s-oauth-login", f.Vendor)
	if err := f.overseasCall(ctx, path, body, &data); err != nil {
		return nil, err
	}
	if data.AccessToken == "" {
		return nil, errors.New("sign-in returned no access token")
	}
	uid := strings.TrimSpace(data.UserID)
	if uid == "" {
		return nil, errors.New("sign-in returned no user id (a stable identity is required)")
	}
	exp := time.Now().Add(28 * 24 * time.Hour).Unix()
	if data.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second).Unix()
	}
	cr := &Creds{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    exp,
		UID:          uid,
		UserName:     data.UserName,
		DeviceID:     f.deviceID,
		Region:       realmOverseas,
	}
	nick := firstNonEmpty(data.UserName, uid)
	return &model.Account{
		ID:        model.ChannelAutoClaw + ":" + uid,
		Channel:   model.ChannelAutoClaw,
		UID:       uid,
		Nickname:  nick,
		Realm:     realmOverseas,
		CredsJSON: cr.Marshal(),
	}, nil
}

// fail records the first error and releases the flow.
func (f *OAuthFlow) fail(err error) {
	f.mu.Lock()
	if f.err == nil && f.code == "" {
		f.err = err
	}
	f.mu.Unlock()
	f.notify()
}

// notify signals Wait that the flow reached a terminal state. Wait selects on
// f.done, so closing it once is the whole mechanism; there is no waiter list.
func (f *OAuthFlow) notify() {
	f.closeOnce.Do(func() { close(f.done) })
}

// Close releases the listener. Safe to call more than once.
func (f *OAuthFlow) Close() {
	if f.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.server.Shutdown(ctx)
	}
	f.notify()
}

// driver is the flow's back-reference, set by StartOAuth's receiver below.
func (f *OAuthFlow) driver() *Driver { return f.d }

func guidePageHTML(vendor string) string {
	label := "Google"
	if vendor == "zai" {
		label = "z.ai"
	}
	return `<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<title>AutoClaw 海外版登录</title>
<style>
 body{font:15px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;max-width:560px;margin:48px auto;padding:0 20px;color:#1f2328}
 h1{font-size:20px;margin:0 0 8px} p{color:#57606a}
 button{font-size:15px;padding:10px 20px;border:0;border-radius:8px;background:#1f6feb;color:#fff;cursor:pointer}
 button:disabled{background:#8c959f}
 #box{margin:18px 0;padding:16px;border:1px dashed #d0d7de;border-radius:10px;display:none}
 #msg{margin-top:16px;white-space:pre-wrap}
 .err{color:#cf222e}
</style>
<h1>AutoClaw 海外版登录（` + label + `）</h1>
<p>点下面的按钮开始授权。需要滑块验证时，滑块会出现在本页。</p>
<div id="box"><div id="captcha"></div></div>
<button id="go">继续授权</button>
<div id="msg"></div>
<script>
const VENDOR = "` + vendor + `";
const msg = (t, bad) => { const m = document.getElementById("msg"); m.textContent = t; m.className = bad ? "err" : ""; };
let receipt = "";
(async () => {
  try {
    const r = await fetch("/captcha-config", { method: "POST" });
    const cfg = await r.json();
    if (!cfg || !cfg.scene_id) return;
    document.getElementById("box").style.display = "block";
    await new Promise((res, rej) => {
      const s = document.createElement("script");
      s.src = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js";
      s.onload = res; s.onerror = rej; document.head.appendChild(s);
    });
    window.initAliyunCaptcha({
      SceneId: cfg.scene_id, mode: "popup", element: "#captcha",
      button: "#go", captchaVerifyCallback: (p) => {
        receipt = typeof p === "string" ? p : (p && p.captchaVerifyParam) || JSON.stringify(p || {});
        return { captchaResult: true };
      },
    });
  } catch (e) { /* no slider: the gateway will say if it wanted one */ }
})();
document.getElementById("go").addEventListener("click", async () => {
  const b = document.getElementById("go");
  b.disabled = true; msg("正在获取授权页…");
  try {
    const r = await fetch("/open-auth", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ captcha_receipt: receipt }) });
    const j = await r.json();
    if (j.error) { msg(j.error, true); b.disabled = false; return; }
    msg("已跳转到授权页，请在页面上完成登录…");
    location.href = j.url;
  } catch (e) { msg(String(e), true); b.disabled = false; }
});
</script></html>`
}

func resultPageHTML(ok bool, note string) string {
	if ok {
		return `<!doctype html><meta charset="utf-8"><title>登录成功</title>
<body style="font:15px/1.6 system-ui,sans-serif;max-width:520px;margin:56px auto;padding:0 20px">
<h1 style="font-size:20px">✅ 授权成功</h1><p style="color:#57606a">` + note + `</p></body>`
	}
	return `<!doctype html><meta charset="utf-8"><title>登录未完成</title>
<body style="font:15px/1.6 system-ui,sans-serif;max-width:520px;margin:56px auto;padding:0 20px">
<h1 style="font-size:20px">⚠️ 登录未完成</h1><p style="color:#cf222e">` + note + `</p></body>`
}
