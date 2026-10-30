package panel

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/opencodezen"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// BP-218: zen's route apply used the primary_models-FILTERED view as the
// source for its whole-table write-back, so every sync tick silently purged
// every route row outside the operator's primary list — field 2026-10-31:
// the open-design-byok/deepseek-v4.1-flash alias was applied at boot
// ("routes=40 applied", the resolved-view dump even listed it) and was
// answering 404 two minutes later. The write-back must source the FULL
// table and only ever add/remove the zen rows it owns.
func TestBP218ZenApplyPreservesNonPrimaryRows(t *testing.T) {
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.PrimaryModels = []string{"glm-5.3"} // the operator's curated display list
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelZCode, UpstreamModel: "glm-5.3", Priority: 0},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{}, nil, nil)
	g := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{}, st)
	pn := &Panel{cfg: cfg, gw: g, store: st}

	alias := router.Row{Model: "open-design-byok/deepseek-v4.1-flash", Channel: model.ChannelWorkBuddy, UpstreamModel: "deepseek-v4.1-flash", Priority: 0}
	seed := append(rowsToBindings(g.RouteRows()), config.ModelBinding{
		Canonical: alias.Model, Channel: alias.Channel, UpstreamModel: alias.UpstreamModel, Priority: alias.Priority,
	})
	if err := g.ReplaceRoutes(seed); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	cat := opencodezen.NewCatalog(nil, nil, "")
	cat.SeedForTest([]opencodezen.ModelEntry{{ID: "zen-free-1", Candidate: true}})
	SetZen(cat, func(bool) {})
	defer SetZen(nil, func(bool) {})

	if err := pn.zenApplyRoutes(true); err != nil {
		t.Fatalf("zenApplyRoutes: %v", err)
	}

	have := map[string]bool{}
	for _, row := range g.RouteRowsAll() {
		have[row.Model+"|"+row.Channel] = true
	}
	if !have["glm-5.3|"+model.ChannelZCode] {
		t.Fatal("primary row lost by the zen write-back")
	}
	if !have["zen-free-1|"+model.ChannelOpenCodeZen] {
		t.Fatal("zen row not added by the zen write-back")
	}
	if !have["open-design-byok/deepseek-v4.1-flash|"+model.ChannelWorkBuddy] {
		t.Fatal("non-primary row purged by the zen write-back — the source must be the unfiltered table (BP-218)")
	}

	// The persisted setting must agree with the live table: the next boot
	// restores routes from it, so a purged DB row is a permanent loss.
	v, ok, _ := st.GetSetting(KeyRoutes)
	if !ok {
		t.Fatal("routes setting not persisted")
	}
	var rows []router.Row
	if err := json.Unmarshal([]byte(v), &rows); err != nil {
		t.Fatalf("persisted routes: %v", err)
	}
	persisted := map[string]bool{}
	for _, row := range rows {
		persisted[row.Model+"|"+row.Channel] = true
	}
	if !persisted["open-design-byok/deepseek-v4.1-flash|"+model.ChannelWorkBuddy] {
		t.Fatal("non-primary row missing from the persisted routes — it would not survive a restart")
	}
}

// The panel's display curation is unchanged: RouteRows still filters by
// primary_models (that is its documented job); only write-backs moved to the
// unfiltered view.
func TestBP218RouteRowsStillCuratesDisplay(t *testing.T) {
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.PrimaryModels = []string{"glm-5.3"}
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelZCode, UpstreamModel: "glm-5.3", Priority: 0},
		{Canonical: "open-design-byok/deepseek-v4.1-flash", Channel: model.ChannelWorkBuddy, UpstreamModel: "deepseek-v4.1-flash", Priority: 0},
	}
	st := store.NewMemory()
	g := gateway.New(cfg, pool.New(pool.Params{}, nil, nil), router.New(cfg), map[string]driver.Driver{}, st)

	if n := len(g.RouteRows()); n != 1 {
		t.Fatalf("RouteRows (display) = %d rows, want only the primary model", n)
	}
	if n := len(g.RouteRowsAll()); n != 2 {
		t.Fatalf("RouteRowsAll (write-back source) = %d rows, want the full table", n)
	}
}
