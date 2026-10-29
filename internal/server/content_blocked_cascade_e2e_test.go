package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// buildCascadeServer binds one model across zcode (p0, single account) and
// workbuddy (p1) with the single-active switch ON — the 2026-10-21 production
// shape where a ZCode content refusal used to end the request as 400 even
// though WorkBuddy was healthy.
func buildCascadeServer(t *testing.T, zcodeUp, wbUp *httptest.Server) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FallbackWait = config.Duration(300 * time.Millisecond)
	cfg.SingleActivePerChannel = true
	cfg.Channels.Trae.AgentHost = wbUp.URL // unused but must be set
	cfg.Channels.WorkBuddy.ChatBaseCN = wbUp.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = wbUp.URL
	cfg.Channels.ZCode.BaseURL = zcodeUp.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3-flash", Channel: model.ChannelZCode, UpstreamModel: "glm-5.3-flash", Priority: 0},
		{Canonical: "glm-5.3-flash", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3-flash", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second, SingleActivePerChannel: true},
		map[string]int{model.ChannelZCode: 2, model.ChannelWorkBuddy: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	zc, _ := json.Marshal(map[string]any{"access_token": testToken("zc"), "uid": "u1"})
	p.Upsert(&model.Account{ID: "zc-1", Channel: model.ChannelZCode, CredsJSON: zc})
	wb, _ := json.Marshal(map[string]any{"access_token": testToken("wb"), "uid": "u2", "realm": "cn"})
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: wb})

	drivers := map[string]driver.Driver{
		model.ChannelZCode:     zcode.New(cfg.Channels.ZCode, nil),
		model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil),
		model.ChannelTrae:      trae.New(cfg.Channels.Trae, nil),
	}
	r := router.New(cfg)
	gw := gateway.New(cfg, p, r, drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// TestContentBlockedCascadesToNextChannel: a ZCode content-inspection refusal
// (200 + sentinel-shaped business answer) must hand the turn to the next
// binding, not end the request as 400 — the refusal is one upstream's
// inspection, not a property of the payload everywhere (2026-10-21).
func TestContentBlockedCascadesToNextChannel(t *testing.T) {
	var wbHits int
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		sseOK(w)
	}))
	defer wbUp.Close()
	zcUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"error":{"code":3012,"message":"content inspection rejected: 违规内容"}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer zcUp.Close()

	srv := buildCascadeServer(t, zcUp, wbUp)
	body := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from workbuddy after zcode refusal", resp.StatusCode)
	}
	if wbHits != 1 {
		t.Fatalf("workbuddy hits = %d, want 1", wbHits)
	}
}
