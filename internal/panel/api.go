// Panel API handlers: pool state, account import/management, logs, settings,
// task center.
package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// TaskRunner is implemented by the task center (wired by main). Nil until
// then: the task endpoints degrade to listing history.
type TaskRunner interface {
	RunTask(ctx context.Context, taskType, accountID string) (string, error)
	TaskTypes() []string
	// TaskEnabledStates reports each task type's enable switch.
	TaskEnabledStates() map[string]bool
	// SetTaskEnabled flips one task type's switch (persisted by the runner).
	SetTaskEnabled(taskType string, enabled bool) error
	// SetLogRetention hot-updates the ledger/log purge window (panel settings).
	SetLogRetention(d time.Duration)
}

// TaskParamProvider is an optional TaskRunner capability: the runner's daily
// schedule is panel-editable (ticket 25, agentpoints 定时任务参数迁移).
type TaskParamProvider interface {
	TaskScheduleParams() model.TaskParams
	SetTaskScheduleParams(model.TaskParams) error
}

// SetTaskRunner installs the task center.
func (p *Panel) SetTaskRunner(tr TaskRunner) { p.taskRunner = tr }

// --- state ---

type accountView struct {
	ID          string                `json:"id"`
	Channel     string                `json:"channel"`
	Nickname    string                `json:"nickname,omitempty"`
	UID         string                `json:"uid,omitempty"`
	Realm       string                `json:"realm,omitempty"`
	Disabled    bool                  `json:"disabled"`
	Paused      bool                  `json:"paused"`
	InFlight    int                   `json:"in_flight"`
	CooldownEnd string                `json:"cooldown_until,omitempty"`
	ModelCool   []modelCoolView       `json:"model_cooldowns,omitempty"`
	Credits     *model.Credits        `json:"credits,omitempty"`
	Windows     map[string]windowView `json:"windows,omitempty"`
	LastUsed    string                `json:"last_used,omitempty"`
}

// modelCoolView is one unexpired (account, model) rate-limit hold for the
// panel: until = the gateway's routing barrier (earliest retry), reset_at =
// the upstream-stated unlock, reason/message = the short label and upstream
// text (BP-49, agentpoints rate_limited_models parity).
type modelCoolView struct {
	Model   string `json:"model"`
	Until   string `json:"until"`
	ResetAt string `json:"reset_at,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// windowView is one quota window (5h/weekly × gemini/claude ledgers).
type windowView struct {
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetAt           string  `json:"reset_at,omitempty"`
}

func (p *Panel) handleState(w http.ResponseWriter, r *http.Request) {
	pl := p.gw.Pool()
	pl.Lock()
	views := make([]accountView, 0, 8)
	for _, ch := range []string{
		model.ChannelAntigravity, model.ChannelWorkBuddy, model.ChannelTrae,
		model.ChannelZCode, model.ChannelAutoClaw, model.ChannelQoder,
		model.ChannelOpenAICompat, model.ChannelOpenCodeZen,
	} {
		for _, a := range pl.AccountsLocked(ch) {
			v := accountView{
				ID: a.ID, Channel: a.Channel, Nickname: a.Nickname, UID: a.UID, Realm: a.Realm,
				Disabled: a.Disabled, Paused: a.Paused, InFlight: pl.InFlightLocked(a.ID),
			}
			if a.AccountCooldownUntil.After(time.Now()) {
				v.CooldownEnd = a.AccountCooldownUntil.Format(time.RFC3339)
			}
			if len(a.ModelCooldowns) > 0 {
				now := time.Now()
				for m, mc := range a.ModelCooldowns {
					if !mc.Until.After(now) {
						continue
					}
					row := modelCoolView{Model: m, Until: mc.Until.Format(time.RFC3339), Reason: mc.Reason, Message: mc.Message}
					if !mc.ResetAt.IsZero() {
						row.ResetAt = mc.ResetAt.Format(time.RFC3339)
					}
					v.ModelCool = append(v.ModelCool, row)
				}
				sort.Slice(v.ModelCool, func(i, j int) bool { return v.ModelCool[i].Model < v.ModelCool[j].Model })
			}
			if a.Credits.UpdatedAt.After(time.Time{}) {
				c := a.Credits
				v.Credits = &c
			}
			if len(a.Windows) > 0 {
				v.Windows = map[string]windowView{}
				for k, win := range a.Windows {
					v.Windows[k] = windowView{
						RemainingFraction: win.RemainingFraction,
						ResetAt:           win.ResetAt.Format(time.RFC3339),
					}
				}
			}
			if !a.LastUsedAt.IsZero() {
				v.LastUsed = a.LastUsedAt.Format(time.RFC3339)
			}
			views = append(views, v)
		}
	}
	pl.Unlock()

	models := p.gw.ListModels()
	out := map[string]any{
		"accounts":         views,
		"models":           models,
		"workbuddy_global": p.wbGlobalEnabled(),
		"settings":         p.hotSettingsView(),
		"task_types":       p.taskTypes(),
		"service_state":    p.ServiceView(),
		"config_view":      p.configView(),
		"service":          "bargainproxy",
		// model_locks: (channel, model) lockout rows for the panel's
		// model-lock view — which models are cooling, on how many accounts,
		// and when they unlock (ticket 33, agentpoints b3f92dd).
		"model_locks": pl.ModelLockRows(),
	}
	// Open mode: hand the per-process CSRF token to same-origin JS so
	// mutations work without any login.
	out["auth_mode"] = "open"
	out["csrf"] = p.openCSRF()
	writeJSON(w, http.StatusOK, out)
}

// hotSettingsView renders the persisted hot-schedulable settings for the UI.
// configView surfaces restart-required config values for the settings page.
func (p *Panel) configView() map[string]any {
	c := p.cfg
	return map[string]any{
		"max_queue_wait_seconds": c.MaxQueueWait.D().Seconds(),
		"fallback_wait_seconds":  c.FallbackWait.D().Seconds(),
		"session_ttl_seconds":    c.SessionTTL.D().Seconds(),
		"cost_ttl_seconds":       c.CostTTL.D().Seconds(),
		"log_keep_days":          c.LogKeepDays,
		"primary_models":         c.PrimaryModels,
	}
}

func (p *Panel) hotSettingsView() map[string]any {
	out := map[string]any{"prompt_replace_enabled": true}
	if v, ok, _ := p.store.GetSetting(KeyGreedyFloorSecs); ok {
		if n, err := strconv.Atoi(v); err == nil {
			out["greedy_floor_seconds"] = n
		}
	}
	if v, ok, _ := p.store.GetSetting(KeyCooldownBaseSecs); ok {
		if n, err := strconv.Atoi(v); err == nil {
			out["cooldown_soft_base_seconds"] = n
		}
	}
	if v, ok, _ := p.store.GetSetting(KeyMaxInFlight); ok {
		var m map[string]int
		if json.Unmarshal([]byte(v), &m) == nil {
			out["max_in_flight"] = m
		}
	}
	if v, ok, _ := p.store.GetSetting(KeyPromptReplace); ok {
		out["prompt_replace_enabled"] = v != "false"
	}
	if v, ok, _ := p.store.GetSetting("include_disabled_in_tasks"); ok {
		out["include_disabled_in_tasks"] = v != "false"
	}
	if v, ok, _ := p.store.GetSetting(KeyThoughtSigReplay); ok {
		out["thought_signature_replay"] = v == "true"
	}
	return out
}

// --- account import ---

type importRequest struct {
	Channel string          `json:"channel"`
	Label   string          `json:"label,omitempty"`
	Creds   json.RawMessage `json:"creds"`
}

func (p *Panel) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if !decode(w, r, &req) {
		return
	}
	acc, err := normalizeImport(req.Channel, req.Creds, req.Label)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := p.store.SaveAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	p.gw.Pool().Upsert(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": acc.ID, "channel": acc.Channel})
}

// normalizeImport converts a pasted credential blob into a canonical account.
// Accepted inputs are the credential shapes of the source projects plus this
// gateway's canonical shapes.
//
// Identity discipline: the account ID is ALWAYS derived from a stable
// upstream identity (uid / email / JWT subject / API-key hash) — never from
// user-supplied labels or display names. Re-importing the same account
// updates the existing row (the store upserts by ID); the human label only
// ever lands in the nickname field.
func normalizeImport(channel string, raw json.RawMessage, label string) (*model.Account, error) {
	switch channel {
	case model.ChannelAntigravity:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("antigravity creds: %w", err)
		}
		// Normalize google oauth client shapes.
		get := func(keys ...string) string {
			for _, k := range keys {
				if v, ok := m[k].(string); ok && v != "" {
					return v
				}
			}
			return ""
		}
		creds := map[string]any{
			"access_token":  get("access_token", "accessToken"),
			"refresh_token": get("refresh_token", "refreshToken"),
			"project_id":    get("project_id", "projectId"),
			"email":         get("email"),
		}
		if creds["access_token"] == "" {
			return nil, fmt.Errorf("antigravity: access_token missing")
		}
		if exp := get("expiry", "expiry_date"); exp != "" {
			creds["expiry"] = exp
		} else {
			creds["expiry"] = time.Now().Add(time.Hour).Format(time.RFC3339)
		}
		out, _ := json.Marshal(creds)
		// Stable identity: email, else the refresh-token hash (long-lived and
		// per-account), else a hash of the credential blob.
		identity := get("email")
		if identity == "" {
			if rt := get("refresh_token", "refreshToken"); rt != "" {
				identity = shortHash(rt)
			} else {
				identity = shortHash(string(out))
			}
		}
		return &model.Account{
			ID: "antigravity:" + identity, Channel: channel, CredsJSON: out,
			Nickname: firstNonEmpty(label, get("email")),
		}, nil
	case model.ChannelWorkBuddy:
		// Try the legacy panel shape first ({account:{...},auth:{...}}).
		var legacy struct {
			Account struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"account"`
			Auth json.RawMessage `json:"auth"`
		}
		if err := json.Unmarshal(raw, &legacy); err == nil && len(legacy.Auth) > 0 {
			var auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Realm        string `json:"realm"`
				DeviceToken  string `json:"device_token"`
			}
			if err := json.Unmarshal(legacy.Auth, &auth); err == nil && auth.AccessToken != "" {
				if legacy.Account.UID == "" {
					return nil, fmt.Errorf("workbuddy: uid missing in legacy auth file")
				}
				creds, _ := json.Marshal(map[string]any{
					"access_token": auth.AccessToken, "refresh_token": auth.RefreshToken,
					"expires_at": auth.ExpiresAt, "realm": auth.Realm, "device_token": auth.DeviceToken,
					"uid": legacy.Account.UID, "nickname": legacy.Account.Nickname,
				})
				return &model.Account{
					ID:      "workbuddy:" + legacy.Account.UID,
					Channel: channel, CredsJSON: creds, UID: legacy.Account.UID,
					Nickname: firstNonEmpty(label, legacy.Account.Nickname), Realm: auth.Realm,
				}, nil
			}
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("workbuddy creds: %w", err)
		}
		tok, _ := m["access_token"].(string)
		if tok == "" {
			return nil, fmt.Errorf("workbuddy: access_token missing")
		}
		uid, _ := m["uid"].(string)
		if uid == "" {
			// A credential hash is not a stable identity (tokens rotate) and
			// would create duplicate accounts on re-import.
			return nil, fmt.Errorf("workbuddy: uid missing (required for stable identity)")
		}
		realm, _ := m["realm"].(string)
		nickname, _ := m["nickname"].(string)
		out, _ := json.Marshal(m)
		return &model.Account{
			ID:      "workbuddy:" + uid,
			Channel: channel, CredsJSON: out, UID: uid, Realm: realm,
			Nickname: firstNonEmpty(label, nickname),
		}, nil
	case model.ChannelTrae:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("trae creds: %w", err)
		}
		tok, _ := m["access_token"].(string)
		uid, _ := m["uid"].(string)
		if tok == "" {
			return nil, fmt.Errorf("trae: access_token missing")
		}
		if uid == "" {
			return nil, fmt.Errorf("trae: uid missing (required for stable identity)")
		}
		out, _ := json.Marshal(m)
		return &model.Account{
			ID: "trae:" + uid, Channel: channel, CredsJSON: out, UID: uid,
			Nickname: firstNonEmpty(label, stringOr(m["nickname"])),
		}, nil
	case model.ChannelZCode:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("zcode creds: %w", err)
		}
		jwt, _ := m["jwt"].(string)
		if jwt == "" {
			if v, ok := m["access_token"].(string); ok {
				jwt = v
				m["jwt"] = v
			}
		}
		if jwt == "" {
			return nil, fmt.Errorf("zcode: jwt missing")
		}
		out, _ := json.Marshal(m)
		// Stable identity: the JWT subject (user id). The token itself
		// rotates, so hashing it would re-key the account after refresh.
		identity := zcode.JWTSub(jwt)
		if identity == "" {
			identity = shortHash(jwt)
		}
		return &model.Account{
			ID: "zcode:" + identity, Channel: channel, CredsJSON: out,
			Nickname: firstNonEmpty(label, stringOr(m["nickname"])),
		}, nil
	case model.ChannelAutoClaw:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("autoclaw creds: %w", err)
		}
		get := func(keys ...string) string {
			for _, k := range keys {
				if v, ok := m[k].(string); ok && v != "" {
					return v
				}
			}
			return ""
		}
		tok := get("access_token", "accessToken")
		uid := get("uid", "userId")
		if tok == "" || uid == "" {
			return nil, fmt.Errorf("autoclaw: access_token and uid required")
		}
		creds, _ := json.Marshal(map[string]any{
			"access_token":     tok,
			"refresh_token":    get("refresh_token", "refreshToken"),
			"expires_at":       m["expires_at"],
			"uid":              uid,
			"user_name":        get("user_name", "userName"),
			"phone":            get("phone"),
			"device_id":        get("device_id", "deviceId"),
			"region":           "cn",
			"sandbox_id":       get("sandbox_id", "sandboxId"),
			"sandbox_endpoint": get("sandbox_endpoint", "sandboxEndpoint"),
			"end_timestamp":    m["end_timestamp"],
		})
		return &model.Account{
			ID: model.ChannelAutoClaw + ":" + uid, Channel: channel, UID: uid,
			Nickname: firstNonEmpty(label, get("user_name", "userName"), get("phone")),
			Realm:    "cn", CredsJSON: creds,
		}, nil
	case model.ChannelQoder:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("qoder creds: %w", err)
		}
		pat := ""
		if v, ok := m["pat"].(string); ok {
			pat = v
		}
		if pat == "" {
			return nil, fmt.Errorf("qoder: pat required (use the PAT form for account creation)")
		}
		creds, _ := json.Marshal(map[string]any{
			"pat":           pat,
			"name":          firstNonEmpty(label, strOf(m["name"])),
			"machine_id":    strOf(m["machine_id"]),
			"machine_token": strOf(m["machine_token"]),
			"machine_type":  strOf(m["machine_type"]),
		})
		return &model.Account{
			ID: model.ChannelQoder + ":" + shortHash(pat), Channel: channel,
			Nickname: firstNonEmpty(label, strOf(m["name"]), "qoder"), CredsJSON: creds,
		}, nil
	case model.ChannelOpenAICompat:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("compat creds: %w", err)
		}
		key, ok := m["api_key"].(string)
		if !ok || key == "" {
			return nil, fmt.Errorf("compat: api_key missing")
		}
		out, _ := json.Marshal(m)
		return &model.Account{
			ID: "openaicompat:" + shortHash(key), Channel: channel, CredsJSON: out,
			Nickname: firstNonEmpty(label, stringOr(m["base_url"])),
		}, nil
	case model.ChannelOpenCodeZen:
		// The anonymous free lane carries the fixed public bearer, not a
		// credential: one synthetic account per base URL (BP-54).
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		base, _ := m["base_url"].(string)
		if base == "" {
			base = "https://opencode.ai/zen"
		}
		out, _ := json.Marshal(map[string]string{"base_url": base})
		return &model.Account{
			ID: model.ChannelOpenCodeZen + ":default", Channel: channel, CredsJSON: out,
			Nickname: "Zen 免费通道",
		}, nil
	default:
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func shortHash(s string) string {
	h := sha256Sum(s)
	return h[:12]
}

// --- account ops ---

type accountOp struct {
	ID       string `json:"id"`
	Disabled *bool  `json:"disabled,omitempty"`
	Paused   *bool  `json:"paused,omitempty"`
}

func (p *Panel) handleDelete(w http.ResponseWriter, r *http.Request) {
	var op accountOp
	if !decode(w, r, &op) || op.ID == "" {
		return
	}
	if err := p.store.DeleteAccount(op.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	p.gw.Pool().Remove(op.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) handleToggle(w http.ResponseWriter, r *http.Request) {
	var op accountOp
	if !decode(w, r, &op) || op.ID == "" {
		return
	}
	if op.Disabled == nil && op.Paused == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "disabled or paused is required"})
		return
	}
	if op.Disabled != nil {
		p.gw.Pool().SetDisabled(op.ID, *op.Disabled)
	}
	if op.Paused != nil {
		// Paused exits routing only: keep-alive tasks keep running (ticket 32).
		if !p.gw.Pool().SetPaused(op.ID, *op.Paused) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) handleProbe(w http.ResponseWriter, r *http.Request) {
	var op accountOp
	if !decode(w, r, &op) || op.ID == "" {
		return
	}
	acc := p.findAccount(op.ID)
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	drv := p.gw.Driver(acc.Channel)
	prober, ok := drv.(driver.Prober)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel does not support probing"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := prober.Probe(ctx, acc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	p.gw.ApplyProbe(acc, res)
	_ = p.store.SaveAccount(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": res.Note})
}

func (p *Panel) findAccount(id string) *model.Account {
	pl := p.gw.Pool()
	pl.Lock()
	defer pl.Unlock()
	for _, ch := range []string{
		model.ChannelAntigravity, model.ChannelWorkBuddy, model.ChannelTrae,
		model.ChannelZCode, model.ChannelAutoClaw, model.ChannelQoder,
		model.ChannelOpenAICompat, model.ChannelOpenCodeZen,
	} {
		for _, a := range pl.AccountsLocked(ch) {
			if a.ID == id {
				return a
			}
		}
	}
	return nil
}

// --- logs / settings / tasks ---

func (p *Panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	logs, err := p.store.RecentRequestLogs(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

type settingsRequest struct {
	WorkBuddyGlobal         *bool          `json:"workbuddy_global,omitempty"`
	GreedyFloorSeconds      *int           `json:"greedy_floor_seconds,omitempty"`
	CooldownSoftBaseSeconds *int           `json:"cooldown_soft_base_seconds,omitempty"`
	MaxInFlight             map[string]int `json:"max_in_flight,omitempty"`
	PromptReplaceEnabled    *bool          `json:"prompt_replace_enabled,omitempty"`
	// IncludeDisabledInTasks: keep-alive tasks also run for disabled
	// accounts (ticket 32; key matches tasks.KeyIncludeDisabledInTasks).
	IncludeDisabledInTasks *bool `json:"include_disabled_in_tasks,omitempty"`
	// ThoughtSignatureReplay: experimental real Gemini thought_signature
	// capture + replay (BP-44). Off by default.
	ThoughtSignatureReplay *bool `json:"thought_signature_replay,omitempty"`
}

// TaskKeepAliveProvider is an optional TaskRunner capability: the
// keep-alive scope for disabled accounts (ticket 32).
type TaskKeepAliveProvider interface {
	SetIncludeDisabledInTasks(bool)
}

func (p *Panel) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if !decode(w, r, &req) {
		return
	}
	// Validate the whole submission before persisting anything.
	if req.GreedyFloorSeconds != nil && *req.GreedyFloorSeconds <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "greedy_floor_seconds must be > 0"})
		return
	}
	if req.CooldownSoftBaseSeconds != nil && *req.CooldownSoftBaseSeconds <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cooldown_soft_base_seconds must be > 0"})
		return
	}
	for ch, n := range req.MaxInFlight {
		if !router.ValidChannel(ch) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown channel " + ch})
			return
		}
		if n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "max_in_flight must be > 0 for " + ch})
			return
		}
	}

	if req.WorkBuddyGlobal != nil {
		val := "false"
		if *req.WorkBuddyGlobal {
			val = "true"
		}
		if err := p.store.SetSetting(KeyWorkBuddyGlobal, val); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.gw.SetWorkBuddyGlobal(*req.WorkBuddyGlobal)
	}
	if req.GreedyFloorSeconds != nil {
		if err := p.store.SetSetting(KeyGreedyFloorSecs, strconv.Itoa(*req.GreedyFloorSeconds)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.gw.SetGreedyFloor(time.Duration(*req.GreedyFloorSeconds) * time.Second)
	}
	if req.CooldownSoftBaseSeconds != nil {
		if err := p.store.SetSetting(KeyCooldownBaseSecs, strconv.Itoa(*req.CooldownSoftBaseSeconds)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.gw.SetSoftCooldownBase(time.Duration(*req.CooldownSoftBaseSeconds) * time.Second)
	}
	if req.MaxInFlight != nil {
		b, _ := json.Marshal(req.MaxInFlight)
		if err := p.store.SetSetting(KeyMaxInFlight, string(b)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for ch, n := range req.MaxInFlight {
			p.gw.SetChannelLimit(ch, n)
		}
	}
	if req.IncludeDisabledInTasks != nil {
		val := "false"
		if *req.IncludeDisabledInTasks {
			val = "true"
		}
		if err := p.store.SetSetting("include_disabled_in_tasks", val); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if prov, ok := p.taskRunner.(TaskKeepAliveProvider); ok {
			prov.SetIncludeDisabledInTasks(*req.IncludeDisabledInTasks)
		}
	}
	if req.PromptReplaceEnabled != nil {
		val := "false"
		if *req.PromptReplaceEnabled {
			val = "true"
		}
		if err := p.store.SetSetting(KeyPromptReplace, val); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.gw.SetPromptReplace(*req.PromptReplaceEnabled)
	}
	if req.ThoughtSignatureReplay != nil {
		val := "false"
		if *req.ThoughtSignatureReplay {
			val = "true"
		}
		if err := p.store.SetSetting(KeyThoughtSigReplay, val); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.gw.SetThoughtSignatureReplay(*req.ThoughtSignatureReplay)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RestoreHotSettings applies persisted panel-managed scheduling settings at
// startup (settings table overrides config; mirrors the workbuddy_global
// flow). Returns human-readable entries for the startup log.
func RestoreHotSettings(gw *gateway.Gateway, st store.Store) []string {
	var applied []string
	if v, ok, _ := st.GetSetting(KeyGreedyFloorSecs); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			gw.SetGreedyFloor(time.Duration(n) * time.Second)
			applied = append(applied, fmt.Sprintf("greedy_floor=%ds", n))
		}
	}
	if v, ok, _ := st.GetSetting(KeyCooldownBaseSecs); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			gw.SetSoftCooldownBase(time.Duration(n) * time.Second)
			applied = append(applied, fmt.Sprintf("cooldown_soft_base=%ds", n))
		}
	}
	if v, ok, _ := st.GetSetting(KeyMaxInFlight); ok {
		var m map[string]int
		if json.Unmarshal([]byte(v), &m) == nil {
			for ch, n := range m {
				if router.ValidChannel(ch) && n > 0 {
					gw.SetChannelLimit(ch, n)
				}
			}
			applied = append(applied, "max_in_flight")
		}
	}
	if v, ok, _ := st.GetSetting(KeyPromptReplace); ok {
		gw.SetPromptReplace(v == "true")
		applied = append(applied, "prompt_replace="+v)
	}
	if v, ok, _ := st.GetSetting(KeyRoutes); ok {
		var rows []router.Row
		if json.Unmarshal([]byte(v), &rows) == nil && len(rows) > 0 {
			if err := gw.ReplaceRoutes(rowsToBindings(rows)); err == nil {
				applied = append(applied, fmt.Sprintf("routes=%d", len(rows)))
			}
		}
	}
	if v, ok, _ := st.GetSetting(KeyThoughtSigReplay); ok {
		gw.SetThoughtSignatureReplay(v == "true")
		applied = append(applied, "thought_signature_replay="+v)
	}
	return applied
}

// --- model routes ---

func (p *Panel) handleRoutesGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"routes": p.gw.RouteRows()})
}

type routesRequest struct {
	Routes []router.Row `json:"routes"`
}

func (p *Panel) handleRoutesPut(w http.ResponseWriter, r *http.Request) {
	var req routesRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Routes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "route table must not be empty"})
		return
	}
	if err := p.gw.ReplaceRoutes(rowsToBindings(req.Routes)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	b, _ := json.Marshal(req.Routes)
	if err := p.store.SetSetting(KeyRoutes, string(b)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "routes": p.gw.RouteRows()})
}

func rowsToBindings(rows []router.Row) []config.ModelBinding {
	out := make([]config.ModelBinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, config.ModelBinding{
			Canonical: row.Model, Channel: row.Channel,
			UpstreamModel: row.UpstreamModel, Priority: row.Priority,
		})
	}
	return out
}

func (p *Panel) handleTasks(w http.ResponseWriter, r *http.Request) {
	runs, err := p.store.RecentTaskRuns(200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	resp := map[string]any{
		"tasks":   runs,
		"types":   p.taskTypes(),
		"enabled": p.taskEnabledStates(),
	}
	if prov, ok := p.taskParamProvider(); ok {
		resp["params"] = prov.TaskScheduleParams()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Panel) taskParamProvider() (TaskParamProvider, bool) {
	if p.taskRunner == nil {
		return nil, false
	}
	prov, ok := p.taskRunner.(TaskParamProvider)
	return prov, ok
}

func (p *Panel) handleTaskParamsSave(w http.ResponseWriter, r *http.Request) {
	prov, ok := p.taskParamProvider()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "task params not available"})
		return
	}
	var req model.TaskParams
	if !decode(w, r, &req) {
		return
	}
	if err := prov.SetTaskScheduleParams(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "params": prov.TaskScheduleParams()})
}

type taskToggleRequest struct {
	TaskType string `json:"task_type"`
	Enabled  bool   `json:"enabled"`
}

func (p *Panel) handleTaskToggle(w http.ResponseWriter, r *http.Request) {
	if p.taskRunner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "task center not available"})
		return
	}
	var req taskToggleRequest
	if !decode(w, r, &req) {
		return
	}
	if req.TaskType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "task_type is required"})
		return
	}
	if err := p.taskRunner.SetTaskEnabled(req.TaskType, req.Enabled); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": p.taskEnabledStates()})
}

type taskRunRequest struct {
	TaskType  string `json:"task_type"`
	AccountID string `json:"account_id,omitempty"` // empty = all eligible
}

func (p *Panel) handleTaskRun(w http.ResponseWriter, r *http.Request) {
	if p.taskRunner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "task center not available"})
		return
	}
	var req taskRunRequest
	if !decode(w, r, &req) {
		return
	}
	if req.TaskType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "task_type is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	detail, err := p.taskRunner.RunTask(ctx, req.TaskType, req.AccountID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "detail": detail})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": detail})
}

func (p *Panel) taskTypes() []string {
	if p.taskRunner == nil {
		return nil
	}
	return p.taskRunner.TaskTypes()
}

func (p *Panel) taskEnabledStates() map[string]bool {
	if p.taskRunner == nil {
		return nil
	}
	return p.taskRunner.TaskEnabledStates()
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body"})
		return false
	}
	// Reject invalid UTF-8 instead of letting encoding/json silently replace
	// bytes with U+FFFD (which would persist corrupted names/credentials).
	if !utf8.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be valid UTF-8"})
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return false
	}
	return true
}

func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
