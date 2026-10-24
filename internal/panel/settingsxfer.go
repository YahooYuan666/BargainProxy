// Settings transfer: export/import the panel-managed settings as one JSON
// bundle so a fresh machine (git clone → empty database) can reach parity
// with the machine that did the tuning (BP-103). The bundle carries the
// settings-table keys the panel writes plus the display whitelist; it never
// carries credentials, accounts, or logs.
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/model"
)

// transferKeys are the settings-table keys a bundle carries. Anything not
// listed here (panel_password_hash, login-flow:*, task ledgers, the Zen
// catalog snapshot) stays machine-local on purpose: keys, secrets, and
// per-machine probe results must not travel.
var transferKeys = []string{
	KeyRoutes,
	"tiers_seen_models",
	KeyGreedyFloorSecs,
	KeyCooldownBaseSecs,
	KeyFirstByteWindowSecs,
	KeyMaxInFlight,
	KeyPromptReplace,
	KeyWorkBuddyGlobal,
	KeyThoughtSigReplay,
	"include_disabled_in_tasks",
	KeyZenAutoSync,
	KeyZenSyncMinutes,
	KeyZenAddToGateway,
}

// transferKeySet resolves to a deduplicated allowlist.
func transferKeySet() map[string]bool {
	set := make(map[string]bool, len(transferKeys))
	for _, k := range transferKeys {
		set[k] = true
	}
	return set
}

// TransferBundle is the export/import wire shape. Keys maps settings key →
// raw value (the exact strings the settings table stores); TaskParams is the
// scheduler's editable schedule (its JSON tags live on model.TaskParams);
// PrimaryModels mirrors config.json's display whitelist.
type TransferBundle struct {
	Version       int               `json:"version"`
	ExportedAt    string            `json:"exported_at"`
	Keys          map[string]string `json:"keys"`
	TaskParams    *model.TaskParams `json:"task_params,omitempty"`
	PrimaryModels []string          `json:"primary_models,omitempty"`
}

const transferVersion = 1

// handleSettingsExport returns the bundle as a download-worthy JSON body.
func (p *Panel) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	bundle := TransferBundle{
		Version:    transferVersion,
		ExportedAt: time.Now().Format(time.RFC3339),
		Keys:       map[string]string{},
	}
	for _, key := range transferKeys {
		if v, ok, _ := p.store.GetSetting(key); ok && v != "" {
			bundle.Keys[key] = v
		}
	}
	if prov, ok := p.taskParamProvider(); ok {
		tp := prov.TaskScheduleParams()
		bundle.TaskParams = &tp
	}
	bundle.PrimaryModels = p.cfg.PrimaryModels
	w.Header().Set("Content-Disposition", `attachment; filename="bargainproxy-settings.json"`)
	writeJSON(w, http.StatusOK, bundle)
}

// handleSettingsImport applies a bundle. Only allowlisted keys are accepted
// (an import carrying panel_password_hash or login-flow:* is rejected, not
// trimmed — a bundle that contains them was not produced by the export and
// may be crafted). Nothing is written until every value validates.
func (p *Panel) handleSettingsImport(w http.ResponseWriter, r *http.Request) {
	var bundle TransferBundle
	if !decode(w, r, &bundle) {
		return
	}
	if bundle.Version != transferVersion {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unsupported bundle version %d (want %d)", bundle.Version, transferVersion)})
		return
	}
	allow := transferKeySet()
	for key := range bundle.Keys {
		if !allow[key] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bundle carries non-transferable key " + key + "（请在来源机重新导出）"})
			return
		}
	}
	if bundle.TaskParams != nil {
		if err := validateTaskParams(*bundle.TaskParams); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "task_params: " + err.Error()})
			return
		}
	}
	if v, ok := bundle.Keys[KeyRoutes]; ok {
		if err := validateRoutesJSON(v); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model_routes invalid: " + err.Error()})
			return
		}
	}

	applied := []string{}
	for _, key := range transferKeys {
		v, ok := bundle.Keys[key]
		if !ok || v == "" {
			continue
		}
		if err := p.store.SetSetting(key, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		applied = append(applied, key)
	}
	if bundle.TaskParams != nil {
		if prov, ok := p.taskParamProvider(); ok {
			if err := prov.SetTaskScheduleParams(*bundle.TaskParams); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "task params: " + err.Error()})
				return
			}
			applied = append(applied, "task_params")
		}
	}
	if bundle.PrimaryModels != nil {
		if _, err := p.SaveDynamicParams(0, 0, 0, 0, 0, bundle.PrimaryModels); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "primary_models: " + err.Error()})
			return
		}
		applied = append(applied, "primary_models")
	}
	// One re-apply covers every settings-table knob (routes, limits, windows…)
	// the same way the settings page does after a save.
	for _, line := range RestoreHotSettings(p.gw, p.store) {
		p.logf("settings import: re-applied %s", line)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": applied})
}

func (p *Panel) logf(format string, args ...any) {
	log.Printf("panel: "+format, args...)
}

func validateRoutesJSON(v string) error {
	var rows []struct {
		Model         string `json:"model"`
		Channel       string `json:"channel"`
		UpstreamModel string `json:"upstream_model"`
		Priority      int    `json:"priority"`
	}
	if err := json.Unmarshal([]byte(v), &rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("empty route table")
	}
	for i, row := range rows {
		if strings.TrimSpace(row.Model) == "" || strings.TrimSpace(row.Channel) == "" || strings.TrimSpace(row.UpstreamModel) == "" {
			return fmt.Errorf("row %d has an empty model/channel/upstream_model", i)
		}
	}
	return nil
}

func validateTaskParams(tp model.TaskParams) error {
	for name, hours := range map[string][]int{
		"checkin_hours":  tp.CheckinHours,
		"growth_hours":   tp.GrowthHours,
		"activity_hours": tp.ActivityHours,
		"travel_hours":   tp.TravelHours,
		"blackcat_hours": tp.BlackcatHours,
	} {
		if len(hours) == 0 {
			return fmt.Errorf("%s must not be empty", name)
		}
		for _, h := range hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s hour %d out of range 0-23", name, h)
			}
		}
	}
	for _, m := range []struct {
		v int
		k string
	}{{tp.ProbeMinutes, "probe_minutes"}, {tp.GoogleProbeMinutes, "google_probe_minutes"}, {tp.RefreshMinutes, "refresh_minutes"}} {
		if m.v < 1 || m.v > 1440 {
			return fmt.Errorf("%s must be 1-1440 minutes", m.k)
		}
	}
	return nil
}
