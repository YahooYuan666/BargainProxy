// Panel wiring for the source-update notice (BP-88).
//
// The check is read-only by design: it asks GitHub for the head commit of main
// and compares it with the commit this binary was built from. Nothing here
// downloads, replaces or restarts anything — the repository publishes source
// only, so a notice is the only honest offer.
package panel

import (
	"context"
	"net/http"
	"time"

	"bargainproxy/internal/version"
)

// KeyUpdateCheckEnabled persists the automatic-check switch ("true"/"false").
// Default is on: checking is a single read-only GET with no side effects.
const KeyUpdateCheckEnabled = "update_check_enabled"

// DefaultUpdateCheckStartDelay lets the boot path settle before the first
// check so it does not compete with startup work for the outbound connection.
const DefaultUpdateCheckStartDelay = 20 * time.Second

// SetVersionChecker wires the checker built by the server at boot.
func (p *Panel) SetVersionChecker(c *version.Checker) { p.vcheck = c }

// UpdateCheckEnabled reports the persisted preference (default true).
func (p *Panel) UpdateCheckEnabled() bool {
	if v, ok, _ := p.store.GetSetting(KeyUpdateCheckEnabled); ok {
		return v != "false"
	}
	return true
}

// SetUpdateCheck persists the switch. Disabling it stops the background loop
// too (the loop reads this preference each tick), while the panel's "check
// now" button keeps working — an explicit click is not a background poll.
func (p *Panel) SetUpdateCheck(enabled bool) error {
	return p.store.SetSetting(KeyUpdateCheckEnabled, boolStr(enabled))
}

// VersionState is the read-only snapshot for the panel (never touches the
// network, so a slow or blocked GitHub cannot delay any panel page).
func (p *Panel) VersionState() map[string]any {
	out := map[string]any{
		"auto_check_enabled": p.UpdateCheckEnabled(),
	}
	if p.vcheck == nil {
		out["status"] = version.StatusUnknown
		out["summary"] = "版本检查未初始化。"
		out["update_available"] = false
		out["notice"] = false
		return out
	}
	st := p.vcheck.State()
	out["status"] = st.Status
	out["summary"] = st.Summary
	out["update_available"] = st.UpdateAvailable
	out["notice"] = st.Notice
	out["repo"] = st.Repo
	out["repo_url"] = st.RepoURL
	if !st.CheckedAt.IsZero() {
		out["checked_at"] = st.CheckedAt.Format(time.RFC3339)
	}
	if st.LastError != "" {
		out["last_error"] = st.LastError
	}
	out["build"] = map[string]any{
		"revision": st.Build.Revision,
		"short":    st.Build.Short(),
		"known":    st.Build.Known,
		"modified": st.Build.Modified,
	}
	if st.Remote != nil {
		out["remote"] = map[string]any{
			"sha":   st.Remote.SHA,
			"short": st.Remote.Short(),
			"title": st.Remote.Title,
			"url":   st.Remote.URL,
			"time":  st.Remote.Time.Format(time.RFC3339),
		}
	}
	return out
}

// RunUpdateChecks is the background loop: it checks once at startup (so a
// freshly started gateway can show the notice without the operator clicking
// anything) and then every interval while the preference stays on.
//
// It stops with ctx. Every failure mode is absorbed by the checker, which
// records the error instead of returning it — a flaky network must never take
// down the goroutine or the gateway.
func (p *Panel) RunUpdateChecks(ctx context.Context, interval time.Duration) {
	if p.vcheck == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = version.DefaultInterval
	}
	// Small delay so the boot path (store, pool, drivers) settles first and the
	// check does not compete with startup work for the outbound connection.
	// Overridable so the loop itself is testable.
	delay := p.startDelay
	if delay == 0 {
		delay = DefaultUpdateCheckStartDelay
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	for {
		if p.UpdateCheckEnabled() {
			p.vcheck.Check(ctx, 0) // 0: the loop owns the cadence
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// handleVersionGet serves the cached snapshot.
func (p *Panel) handleVersionGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.VersionState())
}

// handleVersionCheck runs an explicit check now, throttled so an impatient
// click cannot hammer GitHub (the answer cannot change in 30 seconds).
func (p *Panel) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	if p.vcheck == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "版本检查未初始化"})
		return
	}
	p.vcheck.Check(r.Context(), version.MinManualInterval)
	writeJSON(w, http.StatusOK, p.VersionState())
}

// handleUpdateCheckToggle persists the automatic-check switch.
func (p *Panel) handleUpdateCheckToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enabled is required"})
		return
	}
	if err := p.SetUpdateCheck(*req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p.VersionState())
}
