// Persistence for poll-based onboarding flows (BP-51).
//
// The flows map is in-memory, so a gateway restart used to kill a login the
// user had already started — the next poll answered "会话不存在或已过期"
// (user-reported after a delivery restart wiped a ZCode login). Poll-based
// flows (ZCode CLI, WorkBuddy device) only need the upstream poll token, so
// they are sealed into the settings table and reloaded at startup. Loopback-
// callback flows (Google, Trae) cannot survive a restart — their listener is
// gone — and are deliberately not persisted.
package panel

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

const (
	flowSettingPrefix = "login-flow:"
	// flowMaxLifetime hard-caps a sliding flow: active polls keep it alive
	// (the user is present), but a forgotten tab must not keep it forever.
	flowMaxLifetime = 30 * time.Minute
	// flowPersistEvery throttles re-sealing while polls only keep a flow alive.
	flowPersistEvery = time.Minute
)

// flowPersistable: poll-based flows only. "w:" = WorkBuddy device flow,
// "z:" = ZCode CLI flow. "g:"/"t:" are loopback listeners.
func flowPersistable(key string) bool {
	return strings.HasPrefix(key, "w:") || strings.HasPrefix(key, "z:")
}

// persistedFlow is the sealed wire shape (the poll token is a bearer, so the
// blob goes through the credential box, never into a readable setting).
type persistedFlow struct {
	Channel   string    `json:"channel"`
	Realm     string    `json:"realm,omitempty"`
	State     string    `json:"state,omitempty"`
	Status    string    `json:"status"`
	AccountID string    `json:"account_id,omitempty"`
	ErrMsg    string    `json:"err_msg,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Created   time.Time `json:"created,omitempty"`
	// WbState is the WorkBuddy device-flow state; the account lookup needs it
	// on every poll, so it must survive a restart like the rest of the flow.
	WbState string `json:"wb_state,omitempty"`
	// Tok is the already-issued device-flow token. Upstream issues it once, so
	// losing it to a restart loses the login entirely (2026-10-21).
	Tok *persistedWbToken `json:"tok,omitempty"`
}

// persistedWbToken is the sealed device-flow token triplet.
type persistedWbToken struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh,omitempty"`
	ExpiresIn int64  `json:"expires_in,omitempty"`
	Domain    string `json:"domain,omitempty"`
}

// persistFlow snapshots and seals the current state of a flow. Callers must
// NOT hold flowMu.
func (p *Panel) persistFlow(key string) {
	if p.box == nil || !flowPersistable(key) {
		return
	}
	p.flowMu.Lock()
	f := p.flows[key]
	if f == nil || f.server != nil {
		p.flowMu.Unlock()
		return
	}
	snap := persistedFlow{
		Channel: f.channel, Realm: f.realm, State: f.state, Status: f.status,
		AccountID: f.accountID, ErrMsg: f.errMsg, ExpiresAt: f.expiresAt, Created: f.created,
		WbState: f.wbState,
	}
	if f.tok != nil {
		snap.Tok = &persistedWbToken{
			Access: f.tok.access, Refresh: f.tok.refresh,
			ExpiresIn: f.tok.expiresIn, Domain: f.tok.domain,
		}
	}
	f.persistedAt = time.Now()
	p.flowMu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	sealed, err := p.box.Seal(raw)
	if err != nil {
		return
	}
	if err := p.store.SetSetting(flowSettingPrefix+key, sealed); err != nil {
		log.Printf("panel: persist login flow %s: %v", key, err)
	}
}

// dropPersistedFlow clears the stored copy. The store has no delete; an empty
// value reads as absent on load.
func (p *Panel) dropPersistedFlow(key string) {
	if p.box == nil || !flowPersistable(key) {
		return
	}
	if err := p.store.SetSetting(flowSettingPrefix+key, ""); err != nil {
		log.Printf("panel: drop login flow %s: %v", key, err)
	}
}

// touchFlow slides a pending flow's expiry while its UI is actively polling
// (bounded by flowMaxLifetime) and re-persists at most once a minute.
func (p *Panel) touchFlow(key string) {
	p.flowMu.Lock()
	f := p.flows[key]
	if f == nil || f.status != "pending" {
		p.flowMu.Unlock()
		return
	}
	now := time.Now()
	created := f.created
	if created.IsZero() {
		created = now
	}
	next := now.Add(oauthFlowTTL)
	if max := created.Add(flowMaxLifetime); next.After(max) {
		next = max
	}
	moved := next.Sub(f.expiresAt) > time.Minute
	due := now.Sub(f.persistedAt) > flowPersistEvery
	if moved {
		f.expiresAt = next
	}
	p.flowMu.Unlock()
	if moved && due {
		p.persistFlow(key)
	}
}

// loadPersistedFlows restores poll-based flows at panel construction so a
// restart no longer orphans an in-flight login.
func (p *Panel) loadPersistedFlows() {
	if p.box == nil || p.store == nil {
		return
	}
	rows, err := p.store.ListSettings(flowSettingPrefix)
	if err != nil {
		log.Printf("panel: load login flows: %v", err)
		return
	}
	now := time.Now()
	for k, v := range rows {
		key := strings.TrimPrefix(k, flowSettingPrefix)
		if v == "" || !flowPersistable(key) {
			continue
		}
		plain, err := p.box.Open(v)
		if err != nil {
			log.Printf("panel: open login flow %s: %v", key, err)
			continue
		}
		var pf persistedFlow
		if json.Unmarshal(plain, &pf) != nil {
			continue
		}
		// The slide is throttled (flowPersistEvery), so a persisted expiry can
		// trail the live one slightly: allow that much grace instead of
		// dropping an actively-polled flow in the gap.
		if !pf.ExpiresAt.Add(flowPersistEvery).After(now) {
			p.dropPersistedFlow(key)
			continue
		}
		f := &authFlow{
			channel: pf.Channel, realm: pf.Realm, state: pf.State, status: pf.Status,
			accountID: pf.AccountID, errMsg: pf.ErrMsg, expiresAt: pf.ExpiresAt, created: pf.Created,
			wbState: pf.WbState,
		}
		if pf.Tok != nil {
			f.tok = &wbToken{
				access: pf.Tok.Access, refresh: pf.Tok.Refresh,
				expiresIn: pf.Tok.ExpiresIn, domain: pf.Tok.Domain,
			}
		}
		p.flows[key] = f
	}
}
