// ticket 33: an all-accounts model cooldown must answer 400 model_unavailable
// (switch models), not 429 "no available account" (retry).
package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// TestModelBlockedReturns400ModelUnavailable 全池模型冷却 → 400
// model_unavailable（含解封时间），不再 429 误导客户端重试。
func TestModelBlockedReturns400ModelUnavailable(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++ // must never be reached: the pool fails before touching upstream
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.WorkBuddy.ChatBaseCN = up.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = up.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("wb"), "uid": "u"})
	acc := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: creds}
	p.Upsert(acc)
	// Every account is model-cooled for glm-5.3 (6004-style 30min cooldown).
	p.Lock()
	acc.ModelCooldowns = map[string]model.ModelCooldown{"glm-5.3": {Until: time.Now().Add(30 * time.Minute), Reason: "6004 model rate limit"}}
	p.Unlock()

	drivers := map[string]driver.Driver{model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d body=%s, want 400 model_unavailable", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "model_unavailable") {
		t.Fatalf("body must carry class model_unavailable: %s", body)
	}
	if !strings.Contains(string(body), "switch models") {
		t.Fatalf("body must tell the client to switch models: %s", body)
	}
	if hits != 0 {
		t.Fatalf("model-blocked pool must fail fast without touching upstream: hits=%d", hits)
	}
	// The cooling account keeps its model cooldown; no extra account penalty.
	if acc.ConsecutiveFails != 0 || !acc.AccountCooldownUntil.IsZero() {
		t.Fatalf("no account penalty expected: fails=%d cooldown=%v", acc.ConsecutiveFails, acc.AccountCooldownUntil)
	}
}
