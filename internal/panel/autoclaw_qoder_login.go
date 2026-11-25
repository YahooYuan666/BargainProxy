// Panel onboarding for the AutoClaw (phone+SMS) and Qoder (PAT) channels.
// AutoClaw needs no pending flow: send-code is stateless and login carries the
// phone+code pair, so both handlers finish synchronously and persist the
// account immediately.
package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
		Realm string `json:"realm"` // BP-305: "cn"（默认）| "intl"
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	acc, err := d.LoginPAT(ctx, req.PAT, req.Label, req.Realm)
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
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	// Probe which lane each account is really in: every local signal is
	// ambiguous here (one client directory, channel.json is a release channel,
	// an overseas z.ai account signs in with a phone number too). Only the
	// upstream's answer settles it.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	accounts, notes := autoclaw.ImportWithProbe(ctx, d)
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
	// Two accounts that share one device identity are one device to the server,
	// which costs the second one its new-user grants. The import still succeeds
	// — the credentials are valid — but the operator has to know, because the
	// fix is on their side (sign in again, which mints a fresh identity).
	var warnings []string
	for deviceID, owners := range autoclaw.SharedDeviceIDs(accounts) {
		warnings = append(warnings, fmt.Sprintf(
			"%d 个账号共用同一设备身份（%s…）：%s。服务端会把它们当成同一台机器，后导入的那个可能领不到新人福利。",
			len(owners), deviceID[:min(12, len(deviceID))], strings.Join(owners, "、")))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "imported": added, "updated": updated,
		"failed": failed, "notes": notes, "device_warnings": warnings,
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

// ---- AutoClaw overseas sign-in (Google / z.ai) -----------------------------
//
// Three endpoints, mirroring the start/poll shape the other channels use:
//
//	POST /panel/api/accounts/autoclaw/oauth/start  {vendor} -> {flow, url}
//	GET  /panel/api/accounts/autoclaw/oauth/poll?flow=      -> {status, ...}
//	GET  /panel/api/accounts/autoclaw/catalog?uid=          -> live model list
//
// The flow lives in memory between start and poll: it holds a loopback
// listener, so it cannot be serialized, and an abandoned flow is dropped by
// its own timeout rather than by a restart.

// autoClawFlows holds in-flight sign-ins by id.
var autoClawFlows sync.Map // flowID -> *autoclaw.OAuthFlow

func newFlowID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

func (p *Panel) handleAutoClawOAuthStart(w http.ResponseWriter, r *http.Request) {
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	var req struct {
		Vendor string `json:"vendor"` // google (default) | zai
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Vendor) == "" {
		req.Vendor = "zai" // the only sign-in the overseas gateway still serves
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	flow, err := d.StartOAuth(ctx, req.Vendor)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	id := newFlowID()
	autoClawFlows.Store(id, flow)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flow": id, "url": flow.URL, "vendor": flow.Vendor})
}

func (p *Panel) handleAutoClawOAuthPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("flow")
	v, ok := autoClawFlows.Load(id)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "unknown"})
		return
	}
	flow := v.(*autoclaw.OAuthFlow)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	acc, err := flow.Wait(ctx)
	if err != nil {
		// A poll timing out means the operator is still in the browser. It must
		// answer "pending" and leave the flow alive: the panel polls every two
		// seconds, so treating a timeout as failure would tear the listener
		// down before anyone could approve (that was the bug behind the page's
		// "Failed to fetch").
		if errors.Is(err, autoclaw.ErrOAuthPending) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "url": flow.URL})
			return
		}
		autoClawFlows.Delete(id)
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": err.Error()})
		return
	}
	autoClawFlows.Delete(id)
	if err := p.store.SaveAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "failed", "error": err.Error()})
		return
	}
	p.gw.Pool().Upsert(acc)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "id": acc.ID, "nickname": acc.Nickname, "realm": acc.Realm,
	})
}

// handleAutoClawCatalog returns the account's live model catalogue.
//
// This is the fact source for "what does upstream actually serve", shown beside
// the route table. It does not modify routing: one canonical model here can be
// served by several pools and only the route table decides the order.
func (p *Panel) handleAutoClawCatalog(w http.ResponseWriter, r *http.Request) {
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	var target *model.Account
	for _, a := range p.gw.Pool().Accounts(model.ChannelAutoClaw) {
		if uid == "" || a.UID == uid || a.ID == uid {
			target = a
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有可用的 AutoClaw 账号"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	entries, err := d.CatalogFor(ctx, target)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	type row struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Context int      `json:"context_window"`
		Output  int      `json:"max_tokens"`
		Cost    float64  `json:"credit_cost_factor"`
		Input   []string `json:"input"`
		Levels  []string `json:"reasoning_levels"`
	}
	out := make([]row, 0, len(entries))
	for _, e := range entries {
		out = append(out, row{e.ID, e.Name, e.ContextWindow, e.MaxTokens, e.CreditCost, e.Input, e.ReasoningLevel})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": target.Nickname, "realm": target.Realm, "models": out,
	})
}

// handleAutoClawImportOverseas imports ONLY the overseas client's sign-in
// (%APPDATA%\AutoClaw\auth.json).
//
// It exists because one "import" button covering both stores did the wrong
// thing: the operator was adding an overseas account, pressed import, and got
// a domestic one — the domestic store is scanned by the other handler and
// there is no way for a single button to know which one the operator means
// (field 2026-11-25: "导入怎么搞成国内版的导入了").
//
// The lane is still confirmed upstream afterwards, so a store that was
// re-signed into a domestic account cannot be filed as overseas.
func (p *Panel) handleAutoClawImportOverseas(w http.ResponseWriter, r *http.Request) {
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	accounts, notes := autoclaw.ImportOverseasWithProbe(ctx, d)
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "imported": []string{}, "notes": notes,
			"error": "没有读到海外版的登录态：请先在海外版客户端（AutoClawGlobal）里登录一次",
		})
		return
	}
	p.persistImported(w, accounts, notes)
}

// handleAutoClawImportDomestic imports ONLY the domestic client's accounts
// (%APPDATA%\AutoClaw-official\accounts\*).
func (p *Panel) handleAutoClawImportDomestic(w http.ResponseWriter, r *http.Request) {
	d, ok := p.autoClawDriver()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	accounts, notes := autoclaw.ImportWithProbe(ctx, d)
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "imported": []string{}, "notes": notes,
			"error": "没有找到可导入的国内版账号：请先用国内版客户端登录一次",
		})
		return
	}
	p.persistImported(w, accounts, notes)
}

// persistImported is the shared save path for both import kinds.
func (p *Panel) persistImported(w http.ResponseWriter, accounts []autoclaw.ImportedAccount, notes []string) {
	known := map[string]*model.Account{}
	for _, a := range p.gw.Pool().Accounts(model.ChannelAutoClaw) {
		known[a.ID] = a
		if a.UID != "" {
			known[model.ChannelAutoClaw+":"+a.UID] = a
		}
	}
	var added, updated, failed []string
	for _, ia := range accounts {
		acc := ia.Account
		match := matchExisting(known, acc, ia)
		if match != nil {
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
	var warnings []string
	for deviceID, owners := range autoclaw.SharedDeviceIDs(accounts) {
		warnings = append(warnings, fmt.Sprintf(
			"%d 个账号共用同一设备身份（%s…）：%s。服务端会把它们当成同一台机器，后导入的那个可能领不到新人福利。",
			len(owners), deviceID[:min(12, len(deviceID))], strings.Join(owners, "、")))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "imported": added, "updated": updated,
		"failed": failed, "notes": notes, "device_warnings": warnings,
	})
}
