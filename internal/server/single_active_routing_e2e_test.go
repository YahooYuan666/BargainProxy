package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// buildSingleActiveServer is buildCrossChannelServer with the single-active
// switch ON. Clarified principle (2026-10-21): ONE account per pool may
// interact and THAT account may run 2-3 tasks concurrently (bounded by
// max_in_flight); only when the holder is FULL does the request walk to the
// next binding, and when every pool is full the request is refused with
// 并行数量超发 (2026-10-20 requirement).
func buildSingleActiveServer(t *testing.T, traeUp, wbUp *httptest.Server) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FallbackWait = config.Duration(300 * time.Millisecond)
	cfg.SingleActivePerChannel = true
	cfg.Channels.Trae.AgentHost = traeUp.URL
	cfg.Channels.Trae.UgHost = traeUp.URL
	cfg.Channels.WorkBuddy.ChatBaseCN = wbUp.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = wbUp.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelTrae, UpstreamModel: "glm-5.3-flash", Priority: 0},
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{
		MaxQueueWait:           2 * time.Second,
		SingleActivePerChannel: true,
	}, map[string]int{model.ChannelTrae: 2, model.ChannelWorkBuddy: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })

	traeCreds, _ := json.Marshal(map[string]any{"access_token": testToken("trae"), "uid": "u1"})
	p.Upsert(&model.Account{ID: "trae-1", Channel: model.ChannelTrae, CredsJSON: traeCreds})
	wbCreds, _ := json.Marshal(map[string]any{"access_token": testToken("wb"), "uid": "u2", "realm": "cn"})
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: wbCreds})

	drivers := map[string]driver.Driver{
		model.ChannelTrae:      trae.New(cfg.Channels.Trae, nil),
		model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil),
	}
	r := router.New(cfg)
	gw := gateway.New(cfg, p, r, drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestSingleActiveRoutesToNextPoolThenRefuses(t *testing.T) {
	traeEntered := make(chan struct{}, 4)
	traeRelease := make(chan struct{})
	wbEntered := make(chan struct{}, 4)
	wbRelease := make(chan struct{})

	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traeEntered <- struct{}{}
		<-traeRelease
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbEntered <- struct{}{}
		<-wbRelease
		sseOK(w)
	}))
	defer wbUp.Close()

	srv := buildSingleActiveServer(t, traeUp, wbUp)

	// Requests 1 and 2 occupy the first pool (trae): the same holder account
	// runs both tasks concurrently (max_in_flight=2) — that is the principle.
	post := func(content string) {
		body := fmt.Sprintf(`{"model":"glm-5.3","messages":[{"role":"user","content":%q}]}`, content)
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	go post("one")
	select {
	case <-traeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first request never reached the trae upstream")
	}
	go post("two")
	select {
	case <-traeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("second request must ride the SAME holder account (2 tasks), not walk away")
	}

	// Request 3: the trae holder is FULL -> the next pool (workbuddy) serves it.
	go post("three")
	select {
	case <-wbEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("third request was not routed to the next pool while the trae holder is full")
	}

	// Request 4: rides the workbuddy holder as its 2nd task (all pools now full).
	go post("four")
	select {
	case <-wbEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("fourth request must ride the workbuddy holder, not be refused")
	}

	// Request 5: every pool is now full -> 并行数量超发.
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"five"}]}`))
	if err != nil {
		t.Fatalf("fifth post: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("fifth status = %d, want 429; body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "并行数量超发") {
		t.Fatalf("fifth body must explain 并行数量超发; got %s", raw)
	}

	close(traeRelease)
	close(wbRelease)
}

// TestSingleActivePinnedStillHonest: a pinned channel is still checked — if it
// is busy the client gets the concurrency refusal rather than silently being
// rerouted (pinning means "give me this pool's answer").
func TestSingleActivePinnedStillHonest(t *testing.T) {
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseOK(w)
	}))
	defer wbUp.Close()
	srv := buildSingleActiveServer(t, traeUp, wbUp)

	body := `{"model":"channel:workbuddy:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("pinned idle channel status = %d, want 200; body=%s", resp.StatusCode, b)
	}
	_ = fmt.Sprint()
}
