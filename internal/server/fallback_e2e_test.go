package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/driver/openaicompat"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// TestSentinelPassthroughEndToEnd: an upstream 200 body that is exactly the
// sentinel string is surfaced as a content rejection, with no account
// rotation (exactly one upstream call).
func TestSentinelPassthroughEndToEnd(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"DANGEROUS_ACTIVITY_BLOCKED"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.WorkBuddy.ChatBaseCN = up.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("sentinel"), "uid": "u"})
	p.Upsert(&model.Account{ID: "wb-s", Channel: model.ChannelWorkBuddy, CredsJSON: creds})
	drivers := map[string]driver.Driver{model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	defer srv.Close()

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 content rejection", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("sentinel must not rotate accounts: hits=%d", hits)
	}
}

// TestOpenAICompatFallback: antigravity pool cooling cascades to the
// openai-compat binding and the response carries the degraded header.
func TestOpenAICompatFallback(t *testing.T) {
	// Antigravity upstream should never be called.
	agHits := 0
	agUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agHits++
		w.WriteHeader(500)
	}))
	defer agUp.Close()
	// The compatible fallback serves the request.
	compatHits := 0
	compatUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compatHits++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("compat path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken("compat") {
			t.Errorf("compat auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"fallback-ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer compatUp.Close()

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.Antigravity.BaseURL = agUp.URL
	cfg.Channels.Antigravity.DailyURL = agUp.URL
	cfg.Channels.OpenAICompat.Enabled = true
	cfg.Channels.OpenAICompat.BaseURL = compatUp.URL + "/v1"
	cfg.Channels.OpenAICompat.APIKey = testToken("compat")
	cfg.Models = []config.ModelBinding{
		{Canonical: "gemini-3-pro", Channel: model.ChannelAntigravity, UpstreamModel: "gemini-pro-agent", Priority: 0},
		{Canonical: "gemini-3-pro", Channel: model.ChannelOpenAICompat, UpstreamModel: "gemini-3-pro", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{
		model.ChannelAntigravity:  3,
		model.ChannelOpenAICompat: 4,
	}, nil)
	// Antigravity account cooling (quota exhausted until later).
	creds, _ := json.Marshal(map[string]any{"access_token": "t", "project_id": "p",
		"expiry": time.Now().Add(time.Hour).Format(time.RFC3339)})
	acc := &model.Account{ID: "ag-x", Channel: model.ChannelAntigravity, CredsJSON: creds}
	p.Upsert(acc)
	ue := uperr.New(uperr.ClassSoftRate, 429, "quota")
	ue.ResetAt = time.Now().Add(2 * time.Hour)
	p.ApplyResult(acc, "gemini-3-pro", ue)
	// Compat channel account (the fallback endpoint's own credential).
	compatCreds, _ := json.Marshal(map[string]any{"api_key": testToken("compat")})
	p.Upsert(&model.Account{ID: "compat-default", Channel: model.ChannelOpenAICompat, CredsJSON: compatCreds})

	drivers := map[string]driver.Driver{
		model.ChannelAntigravity:  antigravity.New(cfg.Channels.Antigravity, nil),
		model.ChannelOpenAICompat: openaicompat.New(cfg.Channels.OpenAICompat, nil),
	}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	defer srv.Close()

	body := `{"model":"gemini-3-pro","messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Choices) == 0 || out.Choices[0].Message.Content != "fallback-ok" {
		t.Fatalf("fallback content: %+v", out)
	}
	if compatHits != 1 {
		t.Fatalf("compat hits = %d", compatHits)
	}
	if agHits != 0 {
		t.Fatalf("cooled antigravity must not be hit, got %d", agHits)
	}
	if got := resp.Header.Get("X-BP-Served-Channel"); got != model.ChannelOpenAICompat {
		t.Fatalf("degraded header = %q", got)
	}
}
