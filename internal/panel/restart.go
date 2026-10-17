// Dynamic settings: values that used to require a restart are now hot-applied
// (pool wait params, log retention) plus PrimaryModels, which ListModels and
// RouteRows read per request. config.json is still persisted so the values
// survive restarts — but no process restart is needed.
package panel

import (
	"bargainproxy/internal/config"
	"net/http"
	"slices"
	"strings"
	"time"
)

type restartParamsRequest struct {
	QueueWaitSecs    *int      `json:"queue_wait_seconds,omitempty"`
	FallbackWaitSecs *int      `json:"fallback_wait_seconds,omitempty"`
	SessionTTLMin    *int      `json:"session_ttl_minutes,omitempty"`
	CostTTLHours     *int      `json:"cost_ttl_hours,omitempty"`
	LogKeepDays      *int      `json:"log_keep_days,omitempty"`
	PrimaryModels    *[]string `json:"primary_models,omitempty"`
}

// handleRestartParams saves and hot-applies the settings block. The endpoint
// name is historical (it started as restart-required); everything here now
// applies immediately.
func (p *Panel) handleRestartParams(w http.ResponseWriter, r *http.Request) {
	var req restartParamsRequest
	if !decode(w, r, &req) {
		return
	}
	toDur := func(v *int, unit time.Duration) time.Duration {
		if v == nil || *v <= 0 {
			return 0
		}
		return time.Duration(*v) * unit
	}
	var primary []string
	if req.PrimaryModels != nil {
		for _, m := range *req.PrimaryModels {
			if s := strings.TrimSpace(m); s != "" {
				primary = append(primary, s)
			}
		}
		if len(*req.PrimaryModels) == 0 {
			primary = []string{} // explicit clear
		}
	}
	changed, err := p.SaveDynamicParams(
		toDur(req.QueueWaitSecs, time.Second),
		toDur(req.FallbackWaitSecs, time.Second),
		toDur(req.SessionTTLMin, time.Minute),
		toDur(req.CostTTLHours, time.Hour),
		orZero(req.LogKeepDays),
		primary,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed})
}

func orZero(v *int) int {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

// SaveDynamicParams persists the values to config.json (crash survival) and
// hot-applies them to the running pool/tasks/gateway. No restart required.
func (p *Panel) SaveDynamicParams(queueWait, fallbackWait, sessionTTL, costTTL time.Duration, logKeepDays int, primaryModels []string) (bool, error) {
	c := p.cfg
	changed := false
	if queueWait > 0 && queueWait != c.MaxQueueWait.D() {
		c.MaxQueueWait = config.Duration(queueWait)
		changed = true
	}
	if fallbackWait > 0 && fallbackWait != c.FallbackWait.D() {
		c.FallbackWait = config.Duration(fallbackWait)
		changed = true
	}
	if sessionTTL > 0 && sessionTTL != c.SessionTTL.D() {
		c.SessionTTL = config.Duration(sessionTTL)
		changed = true
	}
	if costTTL > 0 && costTTL != c.CostTTL.D() {
		c.CostTTL = config.Duration(costTTL)
		changed = true
	}
	if logKeepDays > 0 && logKeepDays != c.LogKeepDays {
		c.LogKeepDays = logKeepDays
		changed = true
	}
	if primaryModels != nil && !slices.Equal(primaryModels, c.PrimaryModels) {
		c.PrimaryModels = primaryModels
		changed = true
	}
	if !changed {
		return false, nil
	}
	// Hot-apply.
	p.gw.Pool().SetWaitParams(queueWait, sessionTTL, costTTL)
	if p.taskRunner != nil {
		p.taskRunner.SetLogRetention(time.Duration(logKeepDays) * 24 * time.Hour)
	}
	// Persist (atomic write; best effort — a failed write degrades to
	// memory-only hot settings for this process lifetime).
	if p.configPath != "" {
		if err := config.Save(p.configPath, c); err != nil {
			return true, err
		}
	}
	return true, nil
}
