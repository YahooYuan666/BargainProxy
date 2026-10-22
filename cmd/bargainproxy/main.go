// Command bargainproxy is the unified gateway for cheap/free model upstreams.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/driver/openaicompat"
	"bargainproxy/internal/driver/opencodezen"
	"bargainproxy/internal/driver/qoder"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/panel"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
	"bargainproxy/internal/tasks"
	"bargainproxy/internal/version"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config.json")
	setPassword := flag.String("set-password", "", "set the panel password (bcrypt hash stored in the store) and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Master key for the credential envelope.
	key, err := cryptox.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		log.Fatalf("cryptox: %v", err)
	}
	box, err := cryptox.New(key)
	if err != nil {
		log.Fatalf("cryptox: %v", err)
	}

	// Store.
	var st store.Store
	switch cfg.Store {
	case "postgres":
		if cfg.Database == "" {
			log.Fatalf("store: postgres selected but database dsn is empty")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		// First run on a fresh PostgreSQL: create the app database here so the
		// launcher needs neither Go nor client tools (boot autostart).
		if eerr := store.EnsureDatabase(ctx, cfg.Database); eerr != nil {
			log.Printf("store: ensure database: %v", eerr)
		}
		pg, err := store.OpenPostgres(ctx, cfg.Database, box)
		cancel()
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		st = pg
		log.Printf("store: postgres connected")
	default:
		st = store.NewMemory()
		log.Printf("store: memory (no persistence)")
	}

	if *setPassword != "" {
		panelTmp := panel.New(cfg, nil, st, key)
		if err := panelTmp.SetPassword(*setPassword); err != nil {
			log.Fatalf("set-password: %v", err)
		}
		if err := st.Close(); err != nil {
			log.Printf("store close: %v", err)
		}
		log.Printf("panel password updated")
		return
	}

	// Pool with per-channel concurrency limits (upstream hard constraints).
	limits := map[string]int{
		model.ChannelAntigravity:  cfg.Channels.Antigravity.MaxInFlight,
		model.ChannelWorkBuddy:    cfg.Channels.WorkBuddy.MaxInFlight,
		model.ChannelTrae:         1, // hard serial limit
		model.ChannelZCode:        cfg.Channels.ZCode.MaxInFlight,
		model.ChannelAutoClaw:     cfg.Channels.AutoClaw.MaxInFlight,
		model.ChannelQoder:        cfg.Channels.Qoder.MaxInFlight,
		model.ChannelOpenAICompat: 4,
		model.ChannelOpenCodeZen:  2,
	}
	var p *pool.Pool
	onChange := func(acc *model.Account) {
		// Persist scheduling-state changes; ignore errors here (a failed
		// write degrades to memory-only state, never blocks a request).
		if err := st.SaveAccountState(acc); err != nil {
			log.Printf("pool state persistence failed (account=%s): %v", acc.ID, err)
		}
	}
	p = pool.New(pool.Params{
		MaxQueueWait:           cfg.MaxQueueWait.D(),
		SessionTTL:             cfg.SessionTTL.D(),
		CostTTL:                cfg.CostTTL.D(),
		SingleActivePerChannel: cfg.SingleActivePerChannel,
	}, limits, onChange)

	// Load accounts from the store (in-flight counters are volatile; they
	// start at zero because pool.New just built a fresh pool).
	accounts, err := st.LoadAccounts()
	if err != nil {
		log.Fatalf("store: load accounts: %v", err)
	}
	for _, a := range accounts {
		p.Upsert(a)
	}
	log.Printf("pool: %d accounts loaded", len(accounts))

	// Upstream watchdogs (ticket 27): applied process-wide before drivers are
	// built. Restart to change.
	httpx.Configure(cfg.RPCTimeout.D(), cfg.HeaderTimeout.D(), cfg.IdleTimeout.D())

	// Drivers.
	drivers := map[string]driver.Driver{}
	if cfg.Channels.Antigravity.Enabled {
		drivers[model.ChannelAntigravity] = antigravity.New(cfg.Channels.Antigravity, nil)
	}
	if cfg.Channels.WorkBuddy.Enabled {
		drivers[model.ChannelWorkBuddy] = workbuddy.New(cfg.Channels.WorkBuddy, nil)
	}
	if cfg.Channels.Trae.Enabled {
		drivers[model.ChannelTrae] = trae.New(cfg.Channels.Trae, nil)
	}
	if cfg.Channels.ZCode.Enabled {
		drivers[model.ChannelZCode] = zcode.New(cfg.Channels.ZCode, nil)
	}
	if cfg.Channels.AutoClaw.Enabled {
		drivers[model.ChannelAutoClaw] = autoclaw.New(cfg.Channels.AutoClaw, nil)
	}
	if cfg.Channels.Qoder.Enabled {
		qcfg := cfg.Channels.Qoder
		if qcfg.DataDir == "" {
			// The device-fingerprint salt lives with the rest of the runtime
			// data so it survives restarts (see qoder.machineSalt).
			qcfg.DataDir = cfg.DataDir
		}
		drivers[model.ChannelQoder] = qoder.New(qcfg, nil)
	}
	if cfg.Channels.OpenCodeZen.Enabled {
		drivers[model.ChannelOpenCodeZen] = opencodezen.New(cfg.Channels.OpenCodeZen, nil)
		// The free lane is anonymous by design: no login, no credentials —
		// the driver forges its own client identity (disguise headers + gate
		// tools). The pool still needs an account to bind to (Execute filters
		// account-less channels out), so register a synthetic one.
		accounts = ensureAnonymousZenAccount(p, accounts)
	}
	if cfg.Channels.OpenAICompat.Enabled && cfg.Channels.OpenAICompat.BaseURL != "" {
		drivers[model.ChannelOpenAICompat] = openaicompat.New(cfg.Channels.OpenAICompat, nil)
		// The compat channel is configured, not account-based: register a
		// single synthetic pool account so it participates in scheduling.
		def, ok := findAccount(accounts, "openaicompat-default")
		if !ok {
			def = &model.Account{ID: "openaicompat-default", Channel: model.ChannelOpenAICompat, Nickname: "compat default"}
		}
		def.CredsJSON, _ = json.Marshal(map[string]string{"api_key": cfg.Channels.OpenAICompat.APIKey})
		p.Upsert(def)
		if err := st.SaveAccount(def); err != nil {
			log.Printf("compat default reconciliation failed: %v", err)
		}
	}

	// Router + gateway.
	r := router.New(cfg)
	gw := gateway.New(cfg, p, r, drivers, st)

	// WorkBuddy global switch (default off; settings override config).
	if v, ok, _ := st.GetSetting(panel.KeyWorkBuddyGlobal); ok && v == "true" {
		gw.SetWorkBuddyGlobal(true)
		log.Printf("workbuddy: international accounts ENABLED (settings)")
	} else if cfg.WorkBuddyGlobal {
		gw.SetWorkBuddyGlobal(true)
		log.Printf("workbuddy: international accounts ENABLED (config)")
	}

	// Hot-schedulable settings persisted by the panel (greedy floor, soft
	// cooldown base, per-channel limits, prompt replace, route table).
	for _, line := range panel.RestoreHotSettings(gw, st) {
		log.Printf("settings: %s applied", line)
	}

	// Management panel (mounted only when a password is configured).
	// The autostart script path lets the panel register the Windows scheduled
	// task with an absolute scripts\autostart.vbs location.
	if abs, err := filepath.Abs(filepath.Join("scripts", "autostart.vbs")); err == nil {
		panel.SetAutostartScript(abs)
	}
	// OpenCode Zen free-model channel: catalog + panel wiring (BP-54/55).
	var zenCatalog *opencodezen.Catalog
	if cfg.Channels.OpenCodeZen.Enabled {
		zenCatalog = opencodezen.NewCatalog(httpx.RPCClient(), st, cfg.Channels.OpenCodeZen.BaseURL)
		panel.SetZen(zenCatalog, func(enabled bool) {})
		// Lane routing (BP-57): most free models answer on /v1/chat/completions,
		// but a few (muse-spark*) only answer on /v1/responses — the models.dev
		// SDK declared in the catalog is the authority.
		if d, ok := drivers[model.ChannelOpenCodeZen].(*opencodezen.Driver); ok {
			d.SetWireResolver(zenCatalog.Wire)
		}
	}

	pn := panel.New(cfg, gw, st, key)
	if abs, err := filepath.Abs(*configPath); err == nil {
		pn.SetConfigPath(abs)
	}
	// Source-update notice (BP-88): read-only — asks GitHub for the head commit
	// of main and compares it with the commit this binary was built from. It
	// never downloads, replaces or restarts anything. The transport is cloned
	// from the default one so the check honours HTTPS_PROXY like every other
	// outbound call; the checker bounds each fetch with its own timeout.
	pn.SetVersionChecker(version.NewChecker(httpx.ChatClient()))
	updateCtx, stopUpdateChecks := context.WithCancel(context.Background())
	defer stopUpdateChecks()
	go pn.RunUpdateChecks(updateCtx, version.DefaultInterval)
	// Reconcile the boot task with the persisted preference (default ON): a
	// fresh install gets the scheduled task without touching the panel.
	if err := pn.EnsureBootTask(); err != nil {
		log.Printf("panel: boot task ensure: %v", err)
	}
	if pn.Enabled() {
		log.Printf("panel: enabled at http://%s/panel/", cfg.Listen)
	} else {
		log.Printf("panel: disabled (set a password with -set-password to enable)")
	}

	// Task center: background scheduler + panel wiring.
	sched := tasks.New(gw, st, p, log.Default())
	sched.SetLogRetention(cfg.LogRetention.D())
	sched.SetAccountGap(cfg.TaskAccountGap.D())
	sched.SetAccountSpread(cfg.TaskAccountSpread.D(), cfg.TaskAccountSpreadMinGap.D())
	sched.LoadSettings()
	pn.SetTaskRunner(sched)
	// OpenCode Zen automation: the panel drives sync/probe on the scheduler's
	// minute tick (cadence from the persisted settings).
	if zenCatalog != nil {
		sched.SetZenRunner(pn)
		if v, ok, _ := st.GetSetting(panel.KeyZenSyncMinutes); ok && v != "" {
			if n := parseIntSafe(v); n >= 5 && n <= 1440 {
				sched.SetZenSyncMinutes(n)
			}
		}
	}
	sched.Start()
	defer sched.Stop()
	log.Printf("tasks: scheduler started (checkins 09:05/21:05 CST, growth 09:15, probes 30m, refresh 1h, log purge 1h)")

	// HTTP server.
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg, gw).WithPanel(pn).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("bargainproxy listening on http://%s", cfg.Listen)
		for _, line := range r.Describe() {
			log.Printf("route: %s", line)
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	// OpenCode Zen: one availability pass per boot (BP-73). Probing costs a real
	// chat call per model on the anonymous free lane, so it is not scheduled;
	// the panel's "立即探测" covers everything after boot.
	if zenCatalog != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			pn.ZenBootProbe(ctx)
		}()
	}

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	sched.Stop()
	p.Close()
	if err := st.Close(); err != nil {
		log.Printf("store close: %v", err)
	}
}

func parseIntSafe(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

func findAccount(accounts []*model.Account, id string) (*model.Account, bool) {
	for _, a := range accounts {
		if a.ID == id {
			return a, true
		}
	}
	return nil, false
}

// ensureAnonymousZenAccount registers the built-in anonymous account for the
// OpenCode Zen free lane — the same "configured, not account-based" pattern as
// openaicompat. Deliberately not persisted: it carries no credentials, is
// rebuilt on every boot, and must not appear as a user-managed account.
func ensureAnonymousZenAccount(p *pool.Pool, accounts []*model.Account) []*model.Account {
	const id = "opencodezen-anonymous"
	if _, ok := findAccount(accounts, id); ok {
		return accounts
	}
	def := &model.Account{ID: id, Channel: model.ChannelOpenCodeZen, Nickname: "anonymous (free lane)"}
	p.Upsert(def)
	return append(accounts, def)
}
