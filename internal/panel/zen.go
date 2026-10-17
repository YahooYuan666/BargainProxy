// Panel API for the OpenCode free-models page (BP-55): catalog state,
// manual sync/probe, and the gateway-route application.
package panel

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/driver/opencodezen"
	"bargainproxy/internal/model"
	"bargainproxy/internal/router"
)

// Settings keys (persisted, hot-applied, restored at startup).
const (
	KeyZenAutoSync      = "opencodezen_auto_sync"      // "true" | "false"
	KeyZenSyncMinutes   = "opencodezen_sync_minutes"   // int
	KeyZenAddToGateway  = "opencodezen_add_to_gateway" // "true" | "false"
	KeyZenRoutesEnabled = "opencodezen_routes_enabled" // "true" = routes registered
)

// zenHolder wires the catalog/driver built in main into the panel.
var zenHolder struct {
	catalog *opencodezen.Catalog
	setZen  func(enabled bool)
}

// ZenAutoTick implements tasks.ZenRunner: one automatic sync cycle. Reports
// whether it ran (auto mode on).
func (p *Panel) ZenAutoTick(ctx context.Context) bool {
	if p.zenSetting(KeyZenAutoSync, "true") != "true" {
		return false
	}
	_, _ = p.zenSyncNow(ctx)
	return true
}

// ZenProbeTick implements tasks.ZenRunner: probe the candidate set. Reports
// whether it ran.
func (p *Panel) ZenProbeTick(ctx context.Context) bool {
	if p.zenSetting(KeyZenAutoSync, "true") != "true" || p.zenDriver() == nil {
		return false
	}
	okCount, total := p.zenCatalog().ProbeCandidates(ctx, p.zenDriver(), 60*time.Second)
	log.Printf("opencodezen: scheduled probe %d/%d ok", okCount, total)
	// Probe results change the usable set, so the route table must follow
	// immediately — the manual probe action does this too. Without it a model
	// that just recovered stays 404 ("model not found") until the next hourly
	// auto-sync.
	if _, err := p.zenSyncNow(context.Background()); err != nil {
		log.Printf("opencodezen: post-probe sync: %v", err)
	}
	return true
}

// SetZen registers the OpenCode Zen pieces (called from main).
func SetZen(catalog *opencodezen.Catalog, setEnabled func(bool)) {
	zenHolder.catalog = catalog
	zenHolder.setZen = setEnabled
}

func (p *Panel) zenDriver() *opencodezen.Driver {
	if p.gw == nil {
		return nil
	}
	if d, ok := p.gw.Driver(model.ChannelOpenCodeZen).(*opencodezen.Driver); ok {
		return d
	}
	return nil
}

func (p *Panel) zenCatalog() *opencodezen.Catalog { return zenHolder.catalog }

func (p *Panel) zenSetting(key, def string) string {
	if v, ok, _ := p.store.GetSetting(key); ok && v != "" {
		return v
	}
	return def
}

func (p *Panel) zenGatewayBase() string {
	if p.cfg != nil && p.cfg.Listen != "" {
		return "http://" + p.cfg.Listen
	}
	return "http://127.0.0.1:13140"
}

// handleZenState serves the page: snapshot + settings.
func (p *Panel) handleZenState(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"auto_sync":       p.zenSetting(KeyZenAutoSync, "true") == "true",
		"sync_minutes":    60,
		"add_to_gateway":  p.zenSetting(KeyZenAddToGateway, "true") == "true",
		"gateway_base":    p.zenGatewayBase(),
		"channel_enabled": p.cfg != nil && p.cfg.Channels.OpenCodeZen.Enabled,
	}
	if v, ok, _ := p.store.GetSetting(KeyZenSyncMinutes); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out["sync_minutes"] = n
		}
	}
	if c := p.zenCatalog(); c != nil {
		snap := c.Snapshot()
		out["snapshot"] = snap
		okCount := 0
		for _, m := range snap.Models {
			if m.Candidate && m.ProbeStatus == "ok" {
				okCount++
			}
		}
		out["probe_ok"] = okCount
	}
	writeJSON(w, http.StatusOK, out)
}

// zenModelsRequest is the sync/settings mutation payload.
type zenModelsRequest struct {
	Action string `json:"action"` // sync | probe | settings
	// settings fields
	AutoSync     *bool `json:"auto_sync,omitempty"`
	SyncMinutes  *int  `json:"sync_minutes,omitempty"`
	AddToGateway *bool `json:"add_to_gateway,omitempty"`
}

func (p *Panel) handleZenManage(w http.ResponseWriter, r *http.Request) {
	var req zenModelsRequest
	if !decode(w, r, &req) {
		return
	}
	switch req.Action {
	case "settings":
		p.zenSaveSettings(w, &req)
	case "sync":
		detail, err := p.zenSyncNow(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "detail": detail})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": detail})
	case "probe":
		if p.zenDriver() == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "opencodezen driver not enabled"})
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			okCount, total := p.zenCatalog().ProbeCandidates(ctx, p.zenDriver(), 60*time.Second)
			log.Printf("opencodezen: probe finished %d/%d ok", okCount, total)
			// Probe results feed the route table (usable set may have changed).
			if _, err := p.zenSyncNow(context.Background()); err != nil {
				log.Printf("opencodezen: post-probe sync: %v", err)
			}
		}()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": "probing in background"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown action"})
	}
}

func (p *Panel) zenSaveSettings(w http.ResponseWriter, req *zenModelsRequest) {
	apply := []string{}
	if req.AutoSync != nil {
		_ = p.store.SetSetting(KeyZenAutoSync, boolStr(*req.AutoSync))
		apply = append(apply, "auto_sync="+boolStr(*req.AutoSync))
	}
	if req.SyncMinutes != nil {
		if *req.SyncMinutes < 5 || *req.SyncMinutes > 24*60 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sync_minutes must be 5..1440"})
			return
		}
		_ = p.store.SetSetting(KeyZenSyncMinutes, strconv.Itoa(*req.SyncMinutes))
		apply = append(apply, "sync_minutes="+strconv.Itoa(*req.SyncMinutes))
	}
	if req.AddToGateway != nil {
		_ = p.store.SetSetting(KeyZenAddToGateway, boolStr(*req.AddToGateway))
		apply = append(apply, "add_to_gateway="+boolStr(*req.AddToGateway))
		// Gateway membership follows the checkbox immediately.
		if err := p.zenApplyRoutes(*req.AddToGateway); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": apply})
}

// zenUsableModels returns the model ids to expose: candidates probed OK, or
// all candidates when none has been probed yet (first-run visibility).
func (p *Panel) zenUsableModels() []string {
	if c := p.zenCatalog(); c != nil {
		snap := c.Snapshot()
		var okList, all []string
		anyOk := false
		for _, m := range snap.Models {
			if !m.Candidate {
				continue
			}
			all = append(all, m.ID)
			if m.ProbeStatus == "ok" {
				anyOk = true
				okList = append(okList, m.ID)
			}
		}
		if anyOk {
			return okList
		}
		return all
	}
	return nil
}

// zenSyncNow refreshes the catalog and applies the gateway routes (optional).
func (p *Panel) zenSyncNow(ctx context.Context) (string, error) {
	var detail []string
	if c := p.zenCatalog(); c != nil {
		if err := c.Sync(ctx); err != nil {
			// Sync errors are tolerable (last-good state persists) but reported.
			detail = append(detail, "sync: "+err.Error())
		} else {
			detail = append(detail, "catalog refreshed")
		}
	}
	if p.zenSetting(KeyZenAddToGateway, "true") == "true" {
		if err := p.zenApplyRoutes(true); err != nil {
			detail = append(detail, "routes: "+err.Error())
		} else {
			detail = append(detail, "gateway routes applied")
		}
	}
	return strings.Join(detail, "; "), nil
}

// zenApplyRoutes registers/unregisters one binding per usable Zen model in
// the hot route table and persists it through the panel's routes setting
// (same storage the route editor uses, so the two stay consistent).
func (p *Panel) zenApplyRoutes(add bool) error {
	rows := p.gw.RouteRows()
	var kept []router.Row
	for _, row := range rows {
		if row.Channel == model.ChannelOpenCodeZen {
			continue // strip previous zen bindings; re-added below when add
		}
		kept = append(kept, row)
	}
	if add {
		for _, id := range p.zenUsableModels() {
			kept = append(kept, router.Row{
				Model: id, Channel: model.ChannelOpenCodeZen, UpstreamModel: id, Priority: 5,
			})
		}
	}
	if len(kept) == 0 {
		return nil
	}
	if err := p.gw.ReplaceRoutes(rowsToBindings(kept)); err != nil {
		return err
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return p.store.SetSetting(KeyRoutes, string(b))
}
