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

// handleAutoClawImportLocal scans the official AutoClaw clients installed on
// this machine and imports every account it can decrypt.
//
// This is the whole onboarding path: the operator signs in with the official
// desktop client once (CN and/or overseas), then presses one button. No file
// paths, no pasted JSON, no manual key handling.
//
// Two client directories are scanned, and which one an account came from is
// what decides its lane:
//
//	%APPDATA%\AutoClaw-official         -> realm cn
//	%APPDATA%\AutoClaw-overseas-official -> realm global
//
// The credential store is Chromium-shaped and DPAPI-wrapped under the current
// Windows user, so this only works while the gateway runs as the user who
// signed in — which is also what makes it safe.
func (p *Panel) handleAutoClawImportLocal(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.autoClawDriver(); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	accounts, notes := autoclaw.ImportFromLocalClients()
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "imported": []string{}, "notes": notes,
			"error": "没有找到可导入的账号：请先用官方 AutoClaw 客户端登录一次（国内版或国际版均可）",
		})
		return
	}
	// Match against accounts already in the pool by ANY of the person's stable
	// ids. The official client records one person under two id systems (a
	// UUID accountId and a numeric userId) and the SMS login path stores the
	// UUID while the JWT carries the number, so importing blindly registered
	// the same account twice (field 2026-10-24). When a match is found the
	// existing row is UPDATED — same id, refreshed credentials — instead of a
	// second row appearing in the panel.
	known := map[string]*model.Account{}
	for _, a := range p.gw.Pool().Accounts(model.ChannelAutoClaw) {
		known[a.ID] = a
		if a.UID != "" {
			known[model.ChannelAutoClaw+":"+a.UID] = a
		}
	}
	var added, updated []string
	var failed []string
	for _, ia := range accounts {
		acc := ia.Account
		match := matchExisting(known, acc, ia)
		if match != nil {
			// Keep the id the panel already shows (routes and sticky sessions
			// reference it) and only refresh the credentials.
			acc.ID = match.ID
			if acc.Nickname == "" || acc.Nickname == acc.UID {
				acc.Nickname = match.Nickname
			}
		}
		if err := p.store.SaveAccount(acc); err != nil {
			failed = append(failed, acc.Nickname+": "+err.Error())
			continue
		}
		p.gw.Pool().Upsert(acc)
		known[acc.ID] = acc
		label := acc.Nickname + "（" + ia.Realm + "）"
		if match != nil {
			updated = append(updated, label)
		} else {
			added = append(added, label)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "imported": added, "updated": updated,
		"failed": failed, "notes": notes,
	})
}

// matchExisting finds an already-known account for an imported one, trying the
// primary id first and then every alternate id shape the client recorded.
//
// This is what keeps one person from being registered twice. The official
// client records the same account under a UUID accountId AND a numeric userId;
// the SMS login path stores the UUID while the JWT carries the number, so an
// import that only looked at the primary id created a second row for an
// account the panel already had (field 2026-10-24).
func matchExisting(known map[string]*model.Account, acc *model.Account, ia autoclaw.ImportedAccount) *model.Account {
	for _, cand := range append([]string{acc.ID}, altIDsOf(ia)...) {
		if a, ok := known[cand]; ok && a != nil {
			return a
		}
	}
	return nil
}

// altIDsOf returns every id shape the importer saw for this account, as
// pool-style keys.
func altIDsOf(ia autoclaw.ImportedAccount) []string {
	out := make([]string, 0, len(ia.AltIDs))
	for _, id := range ia.AltIDs {
		if id != "" {
			out = append(out, model.ChannelAutoClaw+":"+id)
		}
	}
	return out
}
