package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// The annotation must actually ride the state payload: a workbuddy account
// with a glm-5.3-flash cooldown plus a two-binding route row → both the
// account's model_cooldowns row and the model_locks row carry the autoclaw
// sibling. (Mutation target for the handleState fill — the HTML contract
// above only proves the renderer, not the wiring.)
func TestBP270StateCarriesFallbackToThePanel(t *testing.T) {
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	st := store.NewMemory()
	if err := st.SetSetting(KeyRoutes, `[
		{"model":"glm-5.3-flash","channel":"workbuddy","upstream_model":"glm-5.3-flash","priority":1},
		{"model":"glm-5.3-flash","channel":"autoclaw","upstream_model":"zaicoding_glm-5.3","priority":2}`+
		`]`); err != nil {
		t.Fatalf("seed routes: %v", err)
	}
	p := pool.New(pool.Params{MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 2, model.ChannelAutoClaw: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	creds, _ := json.Marshal(map[string]any{"access_token": "t", "uid": "u1", "realm": "cn"})
	p.Upsert(&model.Account{
		ID: "workbuddy:u1", Channel: model.ChannelWorkBuddy, CredsJSON: creds, UID: "u1",
		ModelCooldowns: map[string]model.ModelCooldown{
			"glm-5.3-flash": {Until: time.Now().Add(time.Hour), Reason: "rate limit"},
		},
	})
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{}, st)
	pn := New(cfg, gw, st, []byte("0123456789abcdef0123456789abcdef"))
	srv := httptest.NewServer(server.New(cfg, gw).WithPanel(pn).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/panel/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Accounts []struct {
			Channel   string `json:"channel"`
			ModelCool []struct {
				Model    string `json:"model"`
				Fallback string `json:"fallback"`
			} `json:"model_cooldowns"`
		} `json:"accounts"`
		Locks []struct {
			Channel  string `json:"channel"`
			Model    string `json:"model"`
			Fallback string `json:"fallback"`
		} `json:"model_locks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	var coolFB, lockFB string
	for _, a := range body.Accounts {
		if a.Channel != model.ChannelWorkBuddy {
			continue
		}
		for _, mc := range a.ModelCool {
			if mc.Model == "glm-5.3-flash" {
				coolFB = mc.Fallback
			}
		}
	}
	for _, l := range body.Locks {
		if l.Channel == model.ChannelWorkBuddy && l.Model == "glm-5.3-flash" {
			lockFB = l.Fallback
		}
	}
	if !strings.Contains(coolFB, "zaicoding_glm-5.3") {
		t.Fatalf("model_cooldowns fallback = %q, want the autoclaw sibling", coolFB)
	}
	if !strings.Contains(lockFB, "zaicoding_glm-5.3") {
		t.Fatalf("model_locks fallback = %q, want the autoclaw sibling", lockFB)
	}
}

// BP-270 (misakano 410398f1d6, PR #137): a limited model that has another
// route binding is not an outage — the request cascades to the sibling. The
// panel says so ("已路由至 X") on the cooldown rows and the lock table, so a
// locked row is not misread as "service down".
func TestBP270RouteFallbackTargets(t *testing.T) {
	rows := []router.Row{
		{Model: "glm-5.3-flash", Channel: "workbuddy", UpstreamModel: "glm-5.3-flash", Priority: 1},
		{Model: "glm-5.3-flash", Channel: "autoclaw", UpstreamModel: "zaicoding_glm-5.3", Priority: 2},
		{Model: "glm-5.3-flash", Channel: "qoder", UpstreamModel: "glm-5.3-flash", Priority: 3},
		// A different client model with a single binding: no sibling.
		{Model: "deepseek-v4.1-flash", Channel: "workbuddy", UpstreamModel: "deepseek-v4.1-flash", Priority: 1},
	}
	fb := routeFallbackTargets(rows)

	// The workbuddy binding is limited → traffic moves to the autoclaw
	// sibling (next priority, different channel AND upstream).
	if got := fb[routeKey{"workbuddy", "glm-5.3-flash"}]; !strings.Contains(got, "zaicoding_glm-5.3") {
		t.Fatalf("workbuddy cooldown fallback = %q, want the autoclaw sibling", got)
	}
	// The autoclaw binding is limited → the first differing sibling by
	// priority is workbuddy's.
	if got := fb[routeKey{"autoclaw", "zaicoding_glm-5.3"}]; got == "" {
		t.Fatal("autoclaw cooldown must carry a sibling annotation")
	}
	// Single-binding model: nothing to route to, empty (no annotation).
	if got := fb[routeKey{"workbuddy", "deepseek-v4.1-flash"}]; got != "" {
		t.Fatalf("single binding must have no fallback, got %q", got)
	}
	// A cooldown keyed by a name that matches no route row: empty.
	if got := fb[routeKey{"workbuddy", "never-routed"}]; got != "" {
		t.Fatalf("unknown key must have no fallback, got %q", got)
	}
}

// Alias route rows (client model ≠ upstream model) must still be matched —
// cooldowns are keyed by the model name the account was driving, which is the
// upstream id on the alias channels.
func TestBP270RouteFallbackMatchesAliasRows(t *testing.T) {
	rows := []router.Row{
		{Model: "glm-5.3", Channel: "zcode", UpstreamModel: "glm-5.3-flash", Priority: 1},
		{Model: "glm-5.3", Channel: "workbuddy", UpstreamModel: "glm-5.3", Priority: 2},
	}
	fb := routeFallbackTargets(rows)
	// The zcode binding (upstream glm-5.3-flash) is limited → the workbuddy
	// sibling answers, named by its upstream id.
	if got := fb[routeKey{"zcode", "glm-5.3-flash"}]; !strings.Contains(got, "glm-5.3") {
		t.Fatalf("zcode alias cooldown fallback = %q, want the workbuddy sibling", got)
	}
}

// The embedded panel renders the annotation on both surfaces: the per-account
// model cooldown box and the model-lock table.
func TestBP270PanelRendersRoutedAnnotation(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"已路由至",
		"row.fallback",
		"r.fallback",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q (BP-270 annotation)", want)
		}
	}
}
