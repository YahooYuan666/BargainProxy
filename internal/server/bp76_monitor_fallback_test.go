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
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/driver/openaicompat"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

func TestBP76CacheMonitoringIndependentOfIngress(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"chat-buffered", "/v1/chat/completions", `{"model":"gemini-3-pro","messages":[{"role":"user","content":"ping"}]}`},
		{"chat-stream", "/v1/chat/completions", `{"model":"gemini-3-pro","stream":true,"messages":[{"role":"user","content":"ping"}]}`},
		{"anthropic-control", "/v1/messages", `{"model":"gemini-3-pro","max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`},
		{"responses-control", "/v1/responses", `{"model":"gemini-3-pro","input":"ping"}`},
		{"gemini-control", "/v1beta/models/gemini-3-pro:generateContent", `{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":2,\"cachedContentTokenCount\":60}}}\n\n")
			}))
			defer up.Close()
			cfg := config.Default()
			cfg.Channels.Antigravity.BaseURL = up.URL
			cfg.Channels.Antigravity.DailyURL = up.URL
			cfg.Channels.Antigravity.TokenURL = up.URL + "/token"
			cfg.Models = []config.ModelBinding{{Canonical: "gemini-3-pro", Channel: model.ChannelAntigravity, UpstreamModel: "gemini-pro-agent"}}
			st := store.NewMemory()
			p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelAntigravity: 3}, func(*model.Account) {})
			creds, _ := json.Marshal(map[string]any{"access_token": "audit-access", "refresh_token": "audit-refresh", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "audit-project"})
			p.Upsert(&model.Account{ID: "audit-google", Channel: model.ChannelAntigravity, CredsJSON: creds})
			gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelAntigravity: antigravity.New(cfg.Channels.Antigravity, nil)}, st)
			srv := httptest.NewServer(server.New(cfg, gw).Handler())
			defer srv.Close()
			resp, err := http.Post(srv.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			logs, err := st.RecentRequestLogs(1)
			if err != nil || len(logs) != 1 {
				t.Fatalf("logs=%d err=%v", len(logs), err)
			}
			if !logs[0].CacheKnown || logs[0].CachedTokens != 60 {
				t.Fatalf("cache usage lost between upstream and log: CacheKnown=%v CachedTokens=%d InputTokens=%d; want true,60,100", logs[0].CacheKnown, logs[0].CachedTokens, logs[0].InputTokens)
			}
		})
	}
}

func TestBP76ConfiguredFallbackKeyOverridesPersistedSyntheticDefault(t *testing.T) {
	gotAuth := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Channels.OpenAICompat = config.OpenAICompatSettings{Enabled: true, BaseURL: up.URL + "/v1", APIKey: "audit-new-config-key"}
	cfg.Models = []config.ModelBinding{{Canonical: "audit-model", Channel: model.ChannelOpenAICompat, UpstreamModel: "audit-model"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelOpenAICompat: 4}, func(*model.Account) {})
	p.Upsert(&model.Account{ID: "openaicompat-default", Channel: model.ChannelOpenAICompat, CredsJSON: []byte(`{"api_key":"audit-old-stored-key"}`)})
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelOpenAICompat: openaicompat.New(cfg.Channels.OpenAICompat, nil)}, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"audit-model","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if auth := <-gotAuth; auth != "Bearer audit-new-config-key" {
		t.Fatalf("persisted synthetic default shadowed rotated channel config: usedOldKey=%v; want latest configured key", auth == "Bearer audit-old-stored-key")
	}
}
