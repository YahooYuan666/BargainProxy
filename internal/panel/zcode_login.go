// ZCode (Z.AI) browser login for the panel: server-side flow proxy. The
// Z.AI OAuth CLI flow is poll-based (not redirect-based): init → authorize
// URL → server polls until the Coding-Plan JWT is ready. Semantics ported
// from the MIT-licensed workbuddy2api-panel reference (internal/zcode/
// gateway/login.go; see NOTICE).
package panel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/model"
)

const (
	zcodeOAuthBase  = "https://zcode.z.ai/api/v1"
	zcodeLoginTTL   = 5 * time.Minute
	zcodePollGapCli = 3 * time.Second
)

// zcodeLoginStart creates an upstream OAuth flow and remembers our poll
// token under a "z:" flow key (same map/TTL discipline as the other flows).
func (p *Panel) zcodeLoginStart(ctx context.Context) (authorizeURL, state string, err error) {
	pollToken := randHex(32)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		zcodeOAuthBase+"/oauth/cli/init", bytes.NewReader([]byte(`{"provider":"zai"}`)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("login init: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("login init HTTP %d: %s", resp.StatusCode, clipBytes(raw, 200))
	}
	var wrap struct {
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil || wrap.Data.FlowID == "" || wrap.Data.AuthorizeURL == "" {
		return "", "", fmt.Errorf("incomplete OAuth flow data")
	}
	key := "z:" + wrap.Data.FlowID
	p.putFlow(key, &authFlow{
		channel:   model.ChannelZCode,
		state:     pollToken, // reuse the state field for the upstream poll bearer
		status:    "pending",
		expiresAt: time.Now().Add(zcodeLoginTTL),
	})
	return wrap.Data.AuthorizeURL, wrap.Data.FlowID, nil
}

// zcodeLoginPoll asks the upstream once; mapping: ready→success (importing
// the JWT), failed→failed, anything else→pending. Double-poll after ready is
// handled by deleting the flow before the long import.
func (p *Panel) zcodeLoginPoll(ctx context.Context, flowID string) map[string]any {
	key := "z:" + flowID
	f, ok := p.getFlow(key)
	if !ok {
		return map[string]any{"status": "failed", "error": "登录会话不存在或已过期，请重新发起"}
	}
	// An active poll keeps the flow alive (sliding TTL, BP-51).
	p.touchFlow(key)
	if f.status != "pending" {
		return flowStatusJSON(f)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		zcodeOAuthBase+"/oauth/cli/poll/"+flowID, nil)
	if err != nil {
		return map[string]any{"status": "pending"}
	}
	req.Header.Set("Authorization", "Bearer "+f.state)
	resp, err := p.hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return map[string]any{"status": "pending"} // a blip is pending
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var wrap struct {
		Data struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
			Token   string `json:"token"`
			User    struct {
				Email string `json:"email"`
			} `json:"user"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return map[string]any{"status": "pending"}
	}
	d := wrap.Data
	switch d.Status {
	case "failed":
		p.finishFlow(key, "failed", "", orDefault(d.Message, d.Reason, "授权失败或被拒绝"))
		return flowStatusJSON(f)
	case "ready":
		p.finishFlow(key, "pending", "", "") // detach before the long import
	default:
		return map[string]any{"status": "pending"}
	}
	if d.Token == "" {
		p.finishFlow(key, "failed", "", "授权结果中没有凭据")
		return map[string]any{"status": "failed", "error": "授权结果中没有凭据"}
	}
	label := orDefault(d.User.Email, "zcode-oauth-login")
	// Identity: the JWT's sub claim is the stable key (driver discipline);
	// email is display-only. Decode without signature verification — the
	// token came straight from the Z.AI poll endpoint over TLS.
	uid := jwtSub(d.Token)
	if uid == "" {
		uid = label
	}
	credJSON, _ := json.Marshal(map[string]any{"jwt": d.Token, "device_mid": ""})
	acc := &model.Account{
		ID:        model.ChannelZCode + ":" + uid,
		Channel:   model.ChannelZCode,
		UID:       uid,
		Nickname:  label,
		CredsJSON: credJSON,
	}
	if err := p.store.SaveAccount(acc); err != nil {
		log.Printf("panel: zcode save failed: %v", err)
		p.finishFlow(key, "failed", "", "保存账号失败")
		return map[string]any{"status": "failed", "error": "保存账号失败"}
	}
	p.gw.Pool().Upsert(acc)
	p.finishFlow(key, "success", acc.ID, "")
	return map[string]any{"status": "success", "account_id": acc.ID}
}

func orDefault(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// jwtSub extracts the "sub" claim from a JWT payload without verification.
// JWT segments are base64url; padding is re-added before std decoding.
func jwtSub(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	s := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Sub
}

// handleZcodeStart starts an upstream OAuth flow (panel POST).
func (p *Panel) handleZcodeStart(w http.ResponseWriter, r *http.Request) {
	authorizeURL, state, err := p.zcodeLoginStart(r.Context())
	if err != nil {
		log.Printf("panel: zcode login init failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": authorizeURL,
		"state":         state,
	})
}

// handleZcodePoll proxies one upstream poll (panel GET, state=flow_id).
func (p *Panel) handleZcodePoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state required"})
		return
	}
	writeJSON(w, http.StatusOK, p.zcodeLoginPoll(r.Context(), state))
}
