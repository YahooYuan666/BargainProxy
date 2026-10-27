// Trae CN browser login for the panel: build the trae.cn authorization URL,
// catch the redirect on a local loopback listener, parse the credential
// parameters, exchange the refresh token and import the account.
//
// Semantics ported from the MIT-licensed workbuddy2api-panel reference
// (internal/trae/server/callback.go + upstream client; see NOTICE): the
// login URL parameter set, the double-encoded JSON callback fields, the
// ExchangeToken/GetUserInfo endpoints and the millisecond-expiry
// normalization.
package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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

	"bargainproxy/internal/model"
)

// Trae login constants (upstream MIT reference; SOLO stable client).
const (
	traeConsoleHost = "https://www.trae.cn"
	traeClientID    = "en1oxy7wnw8j9n" // SOLO stable
	traeIdeVersion  = "0.1.52"
	// traeOAuthHostDefault is the const; traeOAuthHost is a var so tests can
	// point the exchange/userinfo endpoints at a mock (parity with the
	// driver's traeOAuthHost var).
	traeOAuthHostDefault = "https://api.trae.com.cn"
	traeEpExchange       = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeEpUserInfo       = "/cloudide/api/v3/trae/GetUserInfo"
	traeClientUA         = "Trae/" + traeIdeVersion
)

// traeOAuthHost is the exchange/userinfo base; a var for tests.
var traeOAuthHost = traeOAuthHostDefault

// TraeOAuthHostOverride points the Trae exchange/userinfo endpoints at a
// test server (and returns the restore func). Used by panel_test, which
// cannot reach the unexported var.
func TraeOAuthHostOverride(u string) (restore func()) {
	old := traeOAuthHost
	traeOAuthHost = u
	return func() { traeOAuthHost = old }
}

// randomHex32 returns n random bytes hex-encoded (2n chars).
func randomHex32(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// traeMachineTraceID derives the stable login_trace_id (hex16) from the
// machine/device ids, mirroring the reference (tail of the concatenation).
func traeMachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	const n = 16
	if len(h) >= n {
		return h[len(h)-n:]
	}
	return strings.Repeat("0", n-len(h)) + h
}

// buildTraeLoginURL mirrors the reference BuildLoginURL parameter set.
func buildTraeLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", traeClientID)
	v.Set("redirect", "0")
	return traeConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(traeMachineTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(traeIdeVersion) +
		"&x_app_type=stable"
}

// traeCallbackInfo is the parsed credential set from the login redirect.
type traeCallbackInfo struct {
	RefreshToken string
	AccessToken  string // fallback when no refresh token (userJwt.Token)
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64 // unix seconds, userJwt fallback path only
}

// Trae login device identity: the ids generated at flow start ride the login
// URL and must come back into the stored credential blob — the driver sends
// them as X-Device-Id / X-Machine-Id on every request, and a check-in
// without them reads as a script and is answered 9074 (yetone/magpie#808).
// They used to be generated and then thrown away, writing "" into the blob
// (BP-98 TRAE-2).
type traeFlowDevice struct {
	MachineID string
	DeviceID  string
}

// traeJSONParam tolerates the double percent-encoding the login redirect
// applies to its JSON parameters (reference parse_json_param).
func traeJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func traeStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func traeInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch x := m[key].(type) {
	case float64:
		return int64(x)
	case string:
		var n int64
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

// traeNormalizeExpire converts millisecond timestamps to unix seconds
// (values above 1e12 are milliseconds).
func traeNormalizeExpire(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// parseTraeCallback extracts the credential fields from the redirect URL
// (reference ParseCallback: refreshToken first, userJwt fallback).
func parseTraeCallback(rawURL string) (*traeCallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &traeCallbackInfo{RefreshToken: q.Get("refreshToken")}
	userInfo := traeJSONParam(q.Get("userInfo"))
	info.UID = traeStr(userInfo, "UserID")
	info.Nickname = traeStr(userInfo, "ScreenName")
	info.EnterpriseID = traeStr(userInfo, "TenantID")
	userJwt := traeJSONParam(q.Get("userJwt"))
	if info.RefreshToken == "" {
		info.RefreshToken = traeStr(userJwt, "RefreshToken")
	}
	if info.RefreshToken == "" {
		info.AccessToken = traeStr(userJwt, "Token")
		if info.AccessToken == "" {
			return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
		}
		if exp := traeInt64(userJwt, "TokenExpireAt"); exp > 0 {
			info.ExpiresAt = traeNormalizeExpire(exp)
		}
	}
	return info, nil
}

// traeExchange swaps the refresh token for a fresh access token (rotating
// the refresh token). Reference refreshLocked semantics.
func (p *Panel) traeExchange(ctx context.Context, creds *traeCredsWorking) error {
	body := map[string]any{
		"ClientID":     traeClientID,
		"RefreshToken": creds.refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, traeOAuthHost+traeEpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", traeClientUA)
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return fmt.Errorf("exchange read: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("exchange status %d: %s", resp.StatusCode, clipBytes(data, 200))
	}
	var out struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if out.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response")
	}
	creds.accessToken = out.Result.Token
	if out.Result.RefreshToken != "" {
		creds.refreshToken = out.Result.RefreshToken
	}
	if out.Result.TokenExpireAt > 0 {
		creds.expiresAt = traeNormalizeExpire(out.Result.TokenExpireAt)
	} else if out.Result.TokenExpireDuration > 0 {
		creds.expiresAt = time.Now().Add(time.Duration(out.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// traeCredsWorking carries the credential fields through exchange/userinfo.
type traeCredsWorking struct {
	accessToken  string
	refreshToken string
	expiresAt    int64
}

// traeUserInfo fills uid/nickname/enterprise from the authenticated endpoint
// (reference GetUserInfo; requires the access token).
func (p *Panel) traeUserInfo(ctx context.Context, creds *traeCredsWorking) (uid, nickname, enterprise string, err error) {
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": traeIdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, traeOAuthHost+traeEpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", traeClientUA)
	req.Header.Set("X-Cloudide-Token", creds.accessToken)
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return "", "", "", fmt.Errorf("userinfo read: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("userinfo status %d: %s", resp.StatusCode, clipBytes(data, 200))
	}
	var out struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return out.Result.UserID, out.Result.ScreenName, out.Result.EnterpriseID, nil
}

// handleTraeStart builds the login URL with fresh machine/device ids and a
// loopback listener for the redirect.
func (p *Panel) handleTraeStart(w http.ResponseWriter, r *http.Request) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "loopback listener failed: " + err.Error(),
		})
		return
	}
	state := randHex(16)
	callbackURL := "http://" + ln.Addr().String() + "/authorize"
	key := "t:" + state
	machineID := randomHex32(16)
	deviceID := randomHex32(16)

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(cw http.ResponseWriter, cr *http.Request) {
		p.handleTraeCallback(cw, cr, key)
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	p.putFlow(key, &authFlow{
		channel: model.ChannelTrae, state: state, status: "pending",
		expiresAt: time.Now().Add(oauthFlowTTL), server: hs,
		traeDevice: &traeFlowDevice{MachineID: machineID, DeviceID: deviceID},
	})
	time.AfterFunc(oauthFlowTTL, func() { p.expireFlow(key) })

	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": buildTraeLoginURL(machineID, deviceID, callbackURL),
		"state":         state,
	})
}

// handleTraeCallback catches the login redirect: parse → exchange → userinfo
// → import. Unlike the Google flow there is no state round-trip here (the
// trae.cn redirect does not echo our nonce); the one-shot defense is the
// loopback listener itself, bound to a fresh random port per flow and shut
// down when the flow finishes or expires.
func (p *Panel) handleTraeCallback(w http.ResponseWriter, r *http.Request, key string) {
	f, ok := p.getFlow(key)
	if !ok || f.status != "pending" {
		writeOAuthPage(w, false, "会话已失效，请回到控制台重新发起")
		return
	}
	info, err := parseTraeCallback(r.URL.String())
	if err != nil {
		log.Printf("panel: trae callback parse failed: %v", err)
		p.finishFlow(key, "failed", "", "回调解析失败："+err.Error())
		writeOAuthPage(w, false, "授权失败："+html.EscapeString(err.Error()))
		return
	}
	creds := &traeCredsWorking{accessToken: info.AccessToken, refreshToken: info.RefreshToken, expiresAt: info.ExpiresAt}
	if creds.refreshToken != "" {
		if err := p.traeExchange(r.Context(), creds); err != nil {
			log.Printf("panel: trae exchange failed: %v", err)
			p.finishFlow(key, "failed", "", "token 交换失败")
			writeOAuthPage(w, false, "授权失败（token 交换），请重试")
			return
		}
	}
	uid, nick, ent, err := p.traeUserInfo(r.Context(), creds)
	if err != nil {
		// Callback-carried identity is still trustworthy enough to import
		// (the reference imports even when GetUserInfo fails); keep going
		// when the callback provided a uid.
		if info.UID == "" {
			log.Printf("panel: trae userinfo failed: %v", err)
			p.finishFlow(key, "failed", "", "账号信息获取失败")
			writeOAuthPage(w, false, "授权失败（账号信息获取），请重试")
			return
		}
		log.Printf("panel: trae userinfo failed (importing with callback identity): %v", err)
		uid, nick, ent = info.UID, info.Nickname, info.EnterpriseID
	}
	if uid == "" {
		uid = info.UID
	}
	if uid == "" {
		p.finishFlow(key, "failed", "", "回调缺少 uid")
		writeOAuthPage(w, false, "授权失败：回调缺少 uid，无法建立稳定身份")
		return
	}
	// The device pair this flow signed in with rides into the credential
	// blob: the driver sends it as X-Device-Id / X-Machine-Id, and a check-in
	// without a device reads as a script and is answered 9074 (BP-98 TRAE-2;
	// they used to be written as ""). If the flow lost them (older pending
	// flow), fall back to fresh ids so the blob is never empty again.
	dev := f.traeDevice
	if dev == nil {
		dev = &traeFlowDevice{MachineID: randomHex32(16), DeviceID: randomHex32(16)}
	}
	// enterprise is informational only; stash it in the credential blob.
	credJSON, _ := json.Marshal(map[string]any{
		"access_token":  creds.accessToken,
		"refresh_token": creds.refreshToken,
		"uid":           uid,
		"device_id":     dev.DeviceID,
		"machine_id":    dev.MachineID,
		"expires_at":    creds.expiresAt,
		"enterprise_id": ent,
	})
	acc := &model.Account{
		ID:        model.ChannelTrae + ":" + uid,
		Channel:   model.ChannelTrae,
		UID:       uid,
		Nickname:  nick,
		CredsJSON: credJSON,
	}
	if err := p.store.SaveAccount(acc); err != nil {
		log.Printf("panel: trae save failed: %v", err)
		p.finishFlow(key, "failed", "", "保存账号失败")
		writeOAuthPage(w, false, "保存账号失败，请查看服务日志")
		return
	}
	p.gw.Pool().Upsert(acc)
	p.finishFlow(key, "success", acc.ID, "")
	writeOAuthPage(w, true, "添加成功，可关闭此页")
}

// clipBytes bounds a response body for error messages.
func clipBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// handleTraePoll reports flow status for the panel (GET, state query param).
func (p *Panel) handleTraePoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state required"})
		return
	}
	f, ok := p.getFlow("t:" + state)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "授权会话不存在或已过期"})
		return
	}
	writeJSON(w, http.StatusOK, flowStatusJSON(f))
}

// subtleCompare re-exports constant-time comparison for the local helpers.
func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}

// handleTraePaste completes a Trae login from a PASTED redirect URL — the
// fallback for remote/container deployments, where the login redirect goes to
// 127.0.0.1 on the operator's browser and can never reach this host
// (Coding2API e61a22f hit the same gap). The still-pending flow is found by
// channel; the pasted URL replays through the exact callback machinery, so
// exchange/userinfo/import behave identically. The running poll picks the
// finished flow up on its own.
func (p *Panel) handleTraePaste(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 url（请粘贴登录后跳转的完整地址）"})
		return
	}
	key, ok := p.findPendingFlow(model.ChannelTrae)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "没有进行中的 Trae 登录流程，请先点「获取 Trae 登录链接」"})
		return
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSpace(in.URL), nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "地址无法解析：" + err.Error()})
		return
	}
	rec := &pasteCapture{header: http.Header{}}
	p.handleTraeCallback(rec, req, key)
	f, _ := p.getFlow(key)
	out := map[string]any{"ok": rec.code == http.StatusOK && f.status == "success", "status": f.status}
	if f.errMsg != "" {
		out["error"] = f.errMsg
	}
	writeJSON(w, http.StatusOK, out)
}

// findPendingFlow returns the key of the channel's one pending login flow.
func (p *Panel) findPendingFlow(channel string) (string, bool) {
	p.flowMu.Lock()
	defer p.flowMu.Unlock()
	for k, f := range p.flows {
		if f != nil && f.channel == channel && f.status == "pending" && time.Now().Before(f.expiresAt) {
			return k, true
		}
	}
	return "", false
}

// pasteCapture is a minimal http.ResponseWriter so the loopback callback
// machinery can be reused without emitting its HTML page to the paste caller.
type pasteCapture struct {
	header http.Header
	code   int
	buf    bytes.Buffer
}

func (c *pasteCapture) Header() http.Header { return c.header }

func (c *pasteCapture) Write(b []byte) (int, error) { return c.buf.Write(b) }

func (c *pasteCapture) WriteHeader(code int) { c.code = code }
