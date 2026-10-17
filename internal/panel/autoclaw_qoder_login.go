// Panel onboarding for the AutoClaw (phone+SMS) and Qoder (PAT) channels.
// AutoClaw needs no pending flow: send-code is stateless and login carries the
// phone+code pair, so both handlers finish synchronously and persist the
// account immediately.
package panel

import (
	"context"
	"net/http"
	"time"

	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/driver/qoder"
	"bargainproxy/internal/model"
)

func (p *Panel) autoClawDriver() (*autoclaw.Driver, bool) {
	d, ok := p.gw.Driver(model.ChannelAutoClaw).(*autoclaw.Driver)
	return d, ok
}

func (p *Panel) qoderDriver() (*qoder.Driver, bool) {
	d, ok := p.gw.Driver(model.ChannelQoder).(*qoder.Driver)
	return d, ok
}

func (p *Panel) handleAutoClawSendCode(w http.ResponseWriter, r *http.Request) {
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := d.SendCode(ctx, req.Phone); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) handleAutoClawLogin(w http.ResponseWriter, r *http.Request) {
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	acc, err := d.Login(ctx, req.Phone, req.Code, req.Label)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if err := p.store.SaveAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	p.gw.Pool().Upsert(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": acc.ID, "channel": acc.Channel})
}

func (p *Panel) handleQoderPAT(w http.ResponseWriter, r *http.Request) {
	d, ok := p.qoderDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Qoder 渠道未启用"})
		return
	}
	var req struct {
		PAT   string `json:"pat"`
		Label string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	acc, err := d.LoginPAT(ctx, req.PAT, req.Label)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if err := p.store.SaveAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	p.gw.Pool().Upsert(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": acc.ID, "channel": acc.Channel})
}
