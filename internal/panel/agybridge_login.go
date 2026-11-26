package panel

// agybridge onboarding (BP-339). Sign-in follows the upstream agy-acp browser
// flow: the panel shows a Google authorization URL whose redirect target is a
// loopback listener held by THIS process, so the code is exchanged here (PKCE
// verifier never leaves this process) and the resulting token is written into
// the account's isolated home. No code pasting, no `agy acp --login`.

import (
	"encoding/json"
	"net/http"
	"strings"

	"bargainproxy/internal/driver/agybridge"
	"bargainproxy/internal/model"
)

// handleAgybridgeStart registers (or finds) the account row for an email and
// opens a browser sign-in. Returns the flow id and the authorization URL.
func (p *Panel) handleAgybridgeStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要 Google 账号邮箱"})
		return
	}

	raw, _ := json.Marshal(map[string]string{"email": email, "home": ""})
	acc, err := normalizeImport(model.ChannelAgyBridge, raw, req.Label)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := p.store.SaveAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	p.gw.Pool().Upsert(acc)

	drv, ok := p.gw.Driver(model.ChannelAgyBridge).(*agybridge.Driver)
	if !ok || drv == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "agybridge 驱动未注册"})
		return
	}
	flowID, authURL, err := drv.StartBrowserLogin(r.Context(), acc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "发起登录失败：" + err.Error(), "account_id": acc.ID,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id": flowID, "account_id": acc.ID, "url": authURL,
	})
}

// handleAgybridgePoll reports one login flow's state. The account is usable as
// soon as the loopback callback stored its token, so a success here means the
// bridge pool entry is ready.
func (p *Panel) handleAgybridgePoll(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flow_id")
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "flow_id required"})
		return
	}
	status, errMsg, ok := agybridge.LoginStatus(flowID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "登录会话不存在或已过期"})
		return
	}
	out := map[string]any{"status": status}
	if errMsg != "" {
		out["error"] = errMsg
	}
	writeJSON(w, http.StatusOK, out)
}

// agybridgeEmail pulls the signed-in Google identity out of an agybridge
// account's credential blob so the panel can show WHICH account a row is.
func agybridgeEmail(a *model.Account) string {
	if a == nil || a.Channel != model.ChannelAgyBridge || len(a.CredsJSON) == 0 {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(a.CredsJSON, &c); err != nil {
		return ""
	}
	return strings.TrimSpace(c.Email)
}
