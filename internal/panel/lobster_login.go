// LobsterAI (有道龙虾) browser login for the panel.
//
// The upstream portal accepts an authorization-code callback to any
// http://127.0.0.1:<port>/auth/callback — the condition is http + 127.0.0.1 +
// that exact path + a port. That is the desktop client's own channel, so the
// panel can host its own login without the desktop app installed.
//
// Two details are load-bearing and easy to get wrong:
//
//  1. The parameters must go in the login page's HASH query
//     (…/portal#/login?source=electron&redirect_uri=…&state=…), not the URL
//     query — the portal router only reads the hash.
//  2. source=electron is required, otherwise the portal drops redirect_uri.
//
// The flow is loopback-callback based (like Google/Trae), so it is
// deliberately NOT persisted across a restart: the listener dies with the
// process (see flow_persist.go).

package panel

import (
	"bytes"
	"context"
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

	"bargainproxy/internal/driver/lobster"
	"bargainproxy/internal/model"
)

// lobsterAuthorizeURL appends the loopback callback to the portal's login
// route, inside the hash query.
func lobsterAuthorizeURL(portalBase, callbackURL, state string) string {
	base := strings.TrimRight(portalBase, "/") + "/portal#/login"
	hashAt := strings.Index(base, "#")
	head, hash := base[:hashAt], base[hashAt+1:]
	route, query := hash, ""
	if i := strings.Index(hash, "?"); i >= 0 {
		route, query = hash[:i], hash[i+1:]
	}
	q := url.Values{}
	if query != "" {
		if parsed, err := url.ParseQuery(query); err == nil {
			q = parsed
		}
	}
	q.Set("source", "electron")
	q.Set("redirect_uri", callbackURL)
	q.Set("state", state)
	return head + "#" + route + "?" + q.Encode()
}

// handleLobsterStart opens a loopback listener and returns the authorize URL.
func (p *Panel) handleLobsterStart(w http.ResponseWriter, r *http.Request) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "loopback listener failed: " + err.Error(),
		})
		return
	}
	state := randHex(24)
	callbackURL := "http://" + ln.Addr().String() + "/auth/callback"
	key := "l:" + state

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(cw http.ResponseWriter, cr *http.Request) {
		p.handleLobsterCallback(cw, cr, key)
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	p.putFlow(key, &authFlow{
		channel: model.ChannelLobster, state: state, status: "pending",
		expiresAt: time.Now().Add(oauthFlowTTL), server: hs,
	})
	time.AfterFunc(oauthFlowTTL, func() { p.expireFlow(key) })

	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": lobsterAuthorizeURL(p.cfg.Channels.Lobster.PortalBase, callbackURL, state),
		"state":         state,
	})
}

// handleLobsterCallback exchanges the authorization code for a JWT and imports
// the account.
func (p *Panel) handleLobsterCallback(w http.ResponseWriter, r *http.Request, key string) {
	f, ok := p.getFlow(key)
	if !ok || f.status != "pending" {
		writeOAuthPage(w, false, "会话已失效，请回到控制台重新发起")
		return
	}
	q := r.URL.Query()
	code := q.Get("code")
	if code == "" {
		msg := orDefault(q.Get("error_description"), q.Get("error"), "回调里没有授权码")
		log.Printf("panel: lobster callback without code: %s", msg)
		p.finishFlow(key, "failed", "", msg)
		writeOAuthPage(w, false, "授权失败："+html.EscapeString(msg))
		return
	}
	// The portal echoes our state; a mismatch means the callback did not come
	// from the flow we started.
	if got := q.Get("state"); got != "" && got != f.state {
		log.Printf("panel: lobster callback state mismatch")
		p.finishFlow(key, "failed", "", "回调 state 不匹配")
		writeOAuthPage(w, false, "授权失败：state 不匹配，请重试")
		return
	}

	data, err := p.lobsterExchange(r.Context(), code)
	if err != nil {
		log.Printf("panel: lobster exchange failed: %v", err)
		p.finishFlow(key, "failed", "", "token 交换失败："+err.Error())
		writeOAuthPage(w, false, "授权失败（token 交换），请重试")
		return
	}
	access := stringOr(data["accessToken"])
	if access == "" {
		p.finishFlow(key, "failed", "", "上游没有返回 accessToken")
		writeOAuthPage(w, false, "授权失败：上游没有返回 accessToken")
		return
	}
	// Stable identity: the JWT's yid/sub claim. Hashing the token would
	// re-key the account on every refresh.
	uid := lobster.JWTAudience(access)
	if uid == "" {
		uid = shortHash(access)
	}
	nickname := orDefault(stringOr(data["nickname"]), stringOr(data["userName"]), "lobster-login")

	creds, _ := json.Marshal(map[string]any{
		"access_token":  access,
		"refresh_token": stringOr(data["refreshToken"]),
		"expires_at":    lobster.JWTExp(access),
		"uid":           uid,
		"nickname":      nickname,
	})
	acc := &model.Account{
		ID:        model.ChannelLobster + ":" + uid,
		Channel:   model.ChannelLobster,
		UID:       uid,
		Nickname:  nickname,
		CredsJSON: creds,
	}
	if err := p.store.SaveAccount(acc); err != nil {
		log.Printf("panel: lobster save failed: %v", err)
		p.finishFlow(key, "failed", "", "保存账号失败")
		writeOAuthPage(w, false, "保存账号失败，请查看服务日志")
		return
	}
	p.gw.Pool().Upsert(acc)
	p.finishFlow(key, "success", acc.ID, "")
	writeOAuthPage(w, true, "添加成功，可关闭此页")
}

// lobsterExchange trades the authorization code for tokens. The endpoint uses
// the {code,message,data} envelope and answers HTTP 200 even on failure.
func (p *Panel) lobsterExchange(ctx context.Context, code string) (map[string]any, error) {
	base := strings.TrimRight(p.cfg.Channels.Lobster.ServerBase, "/")
	body, _ := json.Marshal(map[string]any{
		"authCode":      code,
		"firstKeyfrom":  "official",
		"latestKeyfrom": "official",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/auth/exchange", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if v := p.cfg.Channels.Lobster.ClientVersion; v != "" {
		req.Header.Set("X-LobsterAI-Client-Version", v)
	}
	if v := p.cfg.Channels.Lobster.ClientCapabilities; v != "" {
		req.Header.Set("X-LobsterAI-Client-Capabilities", v)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange HTTP %d: %s", resp.StatusCode, clipBytes(raw, 200))
	}
	var env struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("exchange decode: %s", clipBytes(raw, 200))
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s (code %d)", orDefault(env.Message, "上游拒绝登录"), env.Code)
	}
	if env.Data == nil {
		return nil, fmt.Errorf("exchange returned no data")
	}
	return env.Data, nil
}

// handleLobsterPoll reports flow status (GET, state query param).
func (p *Panel) handleLobsterPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state required"})
		return
	}
	key := "l:" + state
	f, ok := p.getFlow(key)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "授权会话不存在或已过期"})
		return
	}
	p.touchFlow(key)
	writeJSON(w, http.StatusOK, flowStatusJSON(f))
}
