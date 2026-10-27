// Package panel provides the embedded web management panel: password login
// (bcrypt), session cookies (HttpOnly, SameSite=Strict), CSRF-protected
// mutations, account import/management, pool overview, model catalog, request
// logs and the task center.
//
// Security posture (from the requirements review):
//   - the panel shares the gateway port but is a separate auth realm
//     (session cookie), fully decoupled from the data-plane API keys;
//   - every mutation requires the X-CSRF-Token header matching the session;
//   - login attempts are rate-limited;
//   - no prompts or completions are stored or displayed anywhere.
package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/store"
	"bargainproxy/internal/version"
)

//go:embed web/index.html
var webFS embed.FS

// Settings keys in the store.
const (
	KeyPasswordHash    = "panel_password_hash"
	KeyWorkBuddyGlobal = "workbuddy_global"
	// Hot-schedulable settings (persisted; restored at startup).
	KeyGreedyFloorSecs  = "greedy_floor_seconds"
	KeyCooldownBaseSecs = "cooldown_soft_base_seconds"
	// KeyFirstByteWindowSecs: global first-byte ceiling for every channel
	// (BP-72). One number replaces the old AutoClaw-only knob; per-channel
	// config overrides still win.
	KeyFirstByteWindowSecs = "first_byte_window_seconds"
	KeyMaxInFlight         = "max_in_flight"          // JSON map: channel -> limit
	KeyPromptReplace       = "prompt_replace_enabled" // "true" | "false"
	// KeyThoughtSigReplay: experimental real Gemini thought_signature capture
	// + replay on the antigravity driver (BP-44). "true" | "false"; default off.
	KeyThoughtSigReplay = "thought_signature_replay"
	KeyRoutes           = "model_routes" // JSON array of route rows
)

// Panel is the management front end.
type Panel struct {
	cfg        *config.Config
	gw         *gateway.Gateway
	store      store.Store
	key        []byte       // HMAC key for session cookies (derived from master key)
	box        *cryptox.Box // seals persisted pending login flows (BP-51)
	configPath string       // config.json path for the restart-required settings
	taskRunner TaskRunner
	hc         *http.Client // onboarding flows (OAuth/device) HTTP client
	vcheck     *version.Checker
	// startDelay overrides the first update check's delay (tests only).
	startDelay time.Duration

	mu        sync.Mutex
	sessions  map[string]*session // sid -> session
	loginFail map[string][]time.Time

	flowMu sync.Mutex
	flows  map[string]*authFlow // pending onboarding flows (in-memory only)

	// Open-mode CSRF (no password configured): one token per process,
	// guarded against other origins by the same-origin policy.
	openMu    sync.Mutex
	openToken string
}

type session struct {
	csrf      string
	expiresAt time.Time
}

// New builds the panel. masterKey derives the session-cookie HMAC key.
func New(cfg *config.Config, gw *gateway.Gateway, st store.Store, masterKey []byte) *Panel {
	derived := sha256.Sum256(append([]byte("bargainproxy-panel-session-v1"), masterKey...))
	box, _ := cryptox.New(masterKey) // nil box = flow persistence degrades off
	p := &Panel{
		cfg:       cfg,
		gw:        gw,
		store:     st,
		key:       derived[:],
		box:       box,
		hc:        &http.Client{Timeout: 20 * time.Second},
		sessions:  map[string]*session{},
		loginFail: map[string][]time.Time{},
		flows:     map[string]*authFlow{},
	}
	// Restore pending poll-based logins (ZCode / WorkBuddy) so a restart no
	// longer orphans a login the user already started (BP-51).
	p.loadPersistedFlows()
	return p
}

// SetConfigPath records the config.json location for restart-required edits.
func (p *Panel) SetConfigPath(path string) { p.configPath = path }

// Enabled reports whether the panel is mounted. It is ALWAYS mounted now:
// with a configured password the panel requires login, without one it runs
// open (local personal gateway, no login step).
func (p *Panel) Enabled() bool {
	return true
}

// openCSRF returns the per-process token guarding mutations in open mode.
func (p *Panel) openCSRF() string {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	if p.openToken == "" {
		p.openToken = randHex(32)
	}
	return p.openToken
}

// SetPassword is a no-op: the panel no longer uses a password (local
// personal gateway). Kept for CLI compatibility.
func (p *Panel) SetPassword(plain string) error {
	return nil
}

// passwordHash always returns empty: the panel is open (no login).

// wbGlobalEnabled resolves the WorkBuddy international switch (store setting
// overrides config).
func (p *Panel) wbGlobalEnabled() bool {
	if v, ok, _ := p.store.GetSetting(KeyWorkBuddyGlobal); ok {
		return v == "true"
	}
	return p.cfg.WorkBuddyGlobal
}

// Mount registers the panel routes.
func (p *Panel) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel/", p.handleIndex)
	mux.HandleFunc("GET /panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusFound)
	})
	mux.HandleFunc("POST /panel/api/login", p.handleLogin)
	mux.HandleFunc("POST /panel/api/logout", p.authed(p.handleLogout, false))
	mux.HandleFunc("GET /panel/api/state", p.authed(p.handleState, false))
	mux.HandleFunc("POST /panel/api/accounts/import", p.authed(p.handleImport, true))
	mux.HandleFunc("POST /panel/api/accounts/delete", p.authed(p.handleDelete, true))
	mux.HandleFunc("POST /panel/api/accounts/toggle", p.authed(p.handleToggle, true))
	mux.HandleFunc("POST /panel/api/accounts/probe", p.authed(p.handleProbe, true))
	mux.HandleFunc("POST /panel/api/accounts/google/start", p.authed(p.handleGoogleStart, true))
	mux.HandleFunc("GET /panel/api/accounts/google/poll", p.authed(p.handleGooglePoll, false))
	mux.HandleFunc("POST /panel/api/accounts/workbuddy/start", p.authed(p.handleWBStart, true))
	mux.HandleFunc("POST /panel/api/accounts/workbuddy/poll", p.authed(p.handleWBPoll, true))
	mux.HandleFunc("POST /panel/api/accounts/trae/start", p.authed(p.handleTraeStart, true))
	mux.HandleFunc("GET /panel/api/accounts/trae/poll", p.authed(p.handleTraePoll, false))
	mux.HandleFunc("POST /panel/api/accounts/trae/paste", p.authed(p.handleTraePaste, true))
	mux.HandleFunc("POST /panel/api/accounts/zcode/start", p.authed(p.handleZcodeStart, true))
	mux.HandleFunc("GET /panel/api/accounts/zcode/poll", p.authed(p.handleZcodePoll, false))
	mux.HandleFunc("POST /panel/api/accounts/autoclaw/sendcode", p.authed(p.handleAutoClawSendCode, true))
	mux.HandleFunc("POST /panel/api/accounts/autoclaw/login", p.authed(p.handleAutoClawLogin, true))
	mux.HandleFunc("POST /panel/api/accounts/autoclaw/import-local", p.authed(p.handleAutoClawImportLocal, true))
	mux.HandleFunc("POST /panel/api/accounts/qoder/pat", p.authed(p.handleQoderPAT, true))
	mux.HandleFunc("POST /panel/api/accounts/lobster/start", p.authed(p.handleLobsterStart, true))
	mux.HandleFunc("GET /panel/api/accounts/lobster/poll", p.authed(p.handleLobsterPoll, false))
	mux.HandleFunc("GET /panel/api/logs", p.authed(p.handleLogs, false))
	mux.HandleFunc("GET /panel/api/tasks", p.authed(p.handleTasks, false))
	mux.HandleFunc("POST /panel/api/tasks/run", p.authed(p.handleTaskRun, true))
	mux.HandleFunc("POST /panel/api/tasks/toggle", p.authed(p.handleTaskToggle, true))
	mux.HandleFunc("POST /panel/api/tasks/params", p.authed(p.handleTaskParamsSave, true))
	mux.HandleFunc("GET /panel/api/routes", p.authed(p.handleRoutesGet, false))
	mux.HandleFunc("PUT /panel/api/routes", p.authed(p.handleRoutesPut, true))
	mux.HandleFunc("POST /panel/api/settings", p.authed(p.handleSettings, true))
	mux.HandleFunc("GET /panel/api/service", p.authed(p.handleServiceGet, false))
	mux.HandleFunc("POST /panel/api/service/autostart", p.authed(p.handleServiceAutostart, true))
	mux.HandleFunc("POST /panel/api/service/watchdog", p.authed(p.handleServiceWatchdog, true))
	mux.HandleFunc("POST /panel/api/config/dynamic", p.authed(p.handleRestartParams, true))
	// Settings transfer (BP-103): export/import the tuned settings bundle.
	mux.HandleFunc("GET /panel/api/settings/export", p.authed(p.handleSettingsExport, false))
	mux.HandleFunc("POST /panel/api/settings/import", p.authed(p.handleSettingsImport, true))
	// WorkBuddy registration (BP-105): one button → gateway models into the
	// local WorkBuddy desktop app.
	mux.HandleFunc("POST /panel/api/workbuddy/register", p.authed(p.handleWorkBuddyRegister, true))
	mux.HandleFunc("GET /panel/api/usage", p.authed(p.handleUsage, false))
	mux.HandleFunc("GET /panel/api/opencodezen", p.authed(p.handleZenState, false))
	mux.HandleFunc("POST /panel/api/opencodezen", p.authed(p.handleZenManage, true))
	mux.HandleFunc("GET /panel/api/tiers", p.authed(p.handleTiers, false))
	mux.HandleFunc("GET /panel/api/accounts/credits", p.authed(p.handleAccountCredits, false))
	mux.HandleFunc("GET /panel/api/packages", p.authed(p.handlePackages, false))
	// Source-update notice (BP-88): read-only, no download/replace/restart.
	mux.HandleFunc("GET /panel/api/version", p.authed(p.handleVersionGet, false))
	mux.HandleFunc("POST /panel/api/version/check", p.authed(p.handleVersionCheck, true))
	mux.HandleFunc("POST /panel/api/version/auto", p.authed(p.handleUpdateCheckToggle, true))
}

func (p *Panel) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// A live dashboard must never be served from a stale cache: an old copy
	// (e.g. from the password era) would legitimately pop the login box again
	// — the 2026-10-16 "强制刷新密码框又回来了" report had a script-failure
	// root cause, but stale HTML is the same class of trap and costs nothing
	// to close.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// --- auth ---

type loginRequest struct {
	Password string `json:"password"`
}

func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if p.loginRateLimited(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts, wait a minute"})
		return
	}
	var req loginRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err := json.Unmarshal(body, &req); err != nil || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "password required"})
		return
	}
	// The panel no longer uses a password: login always succeeds with a
	// session, kept only so old bookmarks/clients don't break.
	sid := randHex(32)
	csrf := randHex(32)
	p.mu.Lock()
	p.sessions[sid] = &session{csrf: csrf, expiresAt: time.Now().Add(24 * time.Hour)}
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "bp_session",
		Value:    sid + "." + sign(p.key, sid),
		Path:     "/panel/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": csrf})
}

func (p *Panel) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("bp_session"); err == nil {
		sid := strings.SplitN(c.Value, ".", 2)[0]
		p.mu.Lock()
		delete(p.sessions, sid)
		p.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "bp_session", Value: "", Path: "/panel/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authed wraps a handler with session auth; mutations additionally require
// the CSRF header.
// authed gates panel endpoints. The panel runs open (local personal
// gateway, no login): mutations still require the per-process CSRF token the
// state endpoint hands to same-origin JS, so cross-origin POSTs are guarded.
func (p *Panel) authed(next http.HandlerFunc, mutation bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mutation {
			tok := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(p.openCSRF())) != 1 {
				// Logged: a stale page after a gateway restart retries with
				// the old token, and without this line the rejection is
				// invisible (2026-10-22: the login poll looked "stopped").
				log.Printf("panel: csrf rejected %s %s", r.Method, r.URL.Path)
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error": "csrf token invalid（网关可能已重启：刷新页面即可，面板会自动换新 token）",
				})
				return
			}
		}
		next(w, r)
	}
}

const (
	loginWindow   = time.Minute
	loginMaxFails = 5
)

func (p *Panel) loginRateLimited(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	kept := p.loginFail[ip][:0]
	for _, t := range p.loginFail[ip] {
		if now.Sub(t) < loginWindow {
			kept = append(kept, t)
		}
	}
	p.loginFail[ip] = kept
	return len(kept) >= loginMaxFails
}

func (p *Panel) recordLoginFail(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loginFail[ip] = append(p.loginFail[ip], time.Now())
}

// --- helpers ---

func sign(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}
