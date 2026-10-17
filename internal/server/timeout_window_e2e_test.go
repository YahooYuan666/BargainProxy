// tickets 27/28: timeout stop-loss and the first-byte fallback window,
// end to end through the server + gateway.
package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/openaicompat"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// withUpstreamTimeouts injects small watchdogs for the process (restored on
// cleanup). Must run before driver construction.
func withUpstreamTimeouts(t *testing.T, header, idle time.Duration) {
	t.Helper()
	httpx.Configure(0, header, idle)
	t.Cleanup(func() {
		httpx.Configure(httpx.DefaultRPCTimeout, httpx.DefaultHeaderTimeout, httpx.DefaultIdleTimeout)
	})
}

// stalledHandler counts hits and parks until done is closed (never answering).
// The counter is atomic: the parked handler writes from the server goroutine
// with no ordering edge to the test goroutine (-race clean).
func stalledHandler(done <-chan struct{}, hits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-done:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
}

func healthyCompatHandler(hits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"fallback-ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	})
}

// twoBindingHarness builds a gateway where the primary workbuddy binding is
// served by wbHandler and the openai-compat fallback by compatHandler.
// Returns the test server and the pool (for account-penalty assertions).
func twoBindingHarness(t *testing.T, wbHandler http.Handler, compatHandler http.Handler, fbWait time.Duration) (*httptest.Server, *pool.Pool, *store.Memory) {
	t.Helper()
	wbUp := httptest.NewServer(wbHandler)
	t.Cleanup(wbUp.Close)
	compatUp := httptest.NewServer(compatHandler)
	t.Cleanup(compatUp.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FallbackWait = config.Duration(fbWait)
	cfg.Channels.WorkBuddy.ChatBaseCN = wbUp.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = wbUp.URL
	cfg.Channels.OpenAICompat.Enabled = true
	cfg.Channels.OpenAICompat.BaseURL = compatUp.URL + "/v1"
	cfg.Channels.OpenAICompat.APIKey = testToken("compat")
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3", Priority: 0},
		{Canonical: "glm-5.3", Channel: model.ChannelOpenAICompat, UpstreamModel: "glm-5.3", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{
		model.ChannelWorkBuddy:    2,
		model.ChannelOpenAICompat: 4,
	}, nil)
	wbCreds, _ := json.Marshal(map[string]any{"access_token": testToken("wb"), "uid": "u-wb"})
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: wbCreds})
	compatCreds, _ := json.Marshal(map[string]any{"api_key": testToken("compat")})
	p.Upsert(&model.Account{ID: "compat-default", Channel: model.ChannelOpenAICompat, CredsJSON: compatCreds})

	drivers := map[string]driver.Driver{
		model.ChannelWorkBuddy:    workbuddy.New(cfg.Channels.WorkBuddy, nil),
		model.ChannelOpenAICompat: openaicompat.New(cfg.Channels.OpenAICompat, nil),
	}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv, p, st
}

func postGLM53(t *testing.T, srv *httptest.Server) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	content := ""
	if len(out.Choices) > 0 {
		content = out.Choices[0].Message.Content
	}
	return resp.StatusCode, content
}

func assertWorkbuddyUnpenalized(t *testing.T, p *pool.Pool) {
	t.Helper()
	accs := p.Accounts(model.ChannelWorkBuddy)
	if len(accs) == 0 {
		t.Fatal("workbuddy account missing")
	}
	for _, a := range accs {
		if a.ConsecutiveFails != 0 {
			t.Fatalf("%s: ConsecutiveFails = %d, want 0 (timeout/cancel must not penalize)", a.ID, a.ConsecutiveFails)
		}
		if !a.AccountCooldownUntil.IsZero() {
			t.Fatalf("%s: cooldown = %v, want zero", a.ID, a.AccountCooldownUntil)
		}
	}
}

// TestUpstreamTimeoutCascadesWithoutPenalty: 主渠道不回响应头 → 头超时 →
// 级联 fallback；主渠道账号零惩罚（ticket 27 e2e）。
func TestUpstreamTimeoutCascadesWithoutPenalty(t *testing.T) {
	done := make(chan struct{})
	var wbHits, compatHits atomic.Int64
	withUpstreamTimeouts(t, 100*time.Millisecond, 0)
	srv, p, _ := twoBindingHarness(t, stalledHandler(done, &wbHits), healthyCompatHandler(&compatHits), 5*time.Second)
	// Registered last → runs first on cleanup (LIFO), releasing parked
	// handlers before httptest.Server.Close waits on them.
	t.Cleanup(func() { close(done) })

	status, content := postGLM53(t, srv)
	if status != 200 || content != "fallback-ok" {
		t.Fatalf("status=%d content=%q, want 200 fallback-ok", status, content)
	}
	if got := wbHits.Load(); got != 1 {
		t.Fatalf("workbuddy hits = %d, want 1 (no account rotation)", got)
	}
	if got := compatHits.Load(); got != 1 {
		t.Fatalf("compat hits = %d, want 1", got)
	}
	assertWorkbuddyUnpenalized(t, p)
}

// TestFirstByteWindowCascadesWithoutPenalty: 首包窗口到点（主渠道迟迟不吐
// 第一帧）→ 取消该次尝试 → 级联 fallback；主渠道账号零惩罚（ticket 28）。
func TestFirstByteWindowCascadesWithoutPenalty(t *testing.T) {
	done := make(chan struct{})
	var wbHits, compatHits atomic.Int64
	withUpstreamTimeouts(t, 10*time.Second, 0) // header watchdog far away: the window fires first
	srv, p, _ := twoBindingHarness(t, stalledHandler(done, &wbHits), healthyCompatHandler(&compatHits), 120*time.Millisecond)
	t.Cleanup(func() { close(done) }) // LIFO: release parked handlers before server closes

	start := time.Now()
	status, content := postGLM53(t, srv)
	if status != 200 || content != "fallback-ok" {
		t.Fatalf("status=%d content=%q, want 200 fallback-ok", status, content)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cascade took %v", elapsed)
	}
	if got := compatHits.Load(); got != 1 {
		t.Fatalf("compat hits = %d, want 1", got)
	}
	assertWorkbuddyUnpenalized(t, p)
}

// TestLongGenerationSurvivesFirstByteWindow: ticket 28 回归——首包快、整段
// 生成远超窗口：必须由主渠道完整跑完，不切渠道（修复前 8s 窗口会把 >8s 的
// 生成整体掐断并级联，glm-5.3/kimi-k3 因此全挂）。
func TestLongGenerationSurvivesFirstByteWindow(t *testing.T) {
	var wbHits atomic.Int64
	wbHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fl.Flush()
		time.Sleep(400 * time.Millisecond) // far beyond the 120ms first-byte window
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"b"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
		fl.Flush()
	})
	var compatHits atomic.Int64
	withUpstreamTimeouts(t, 10*time.Second, 0)
	srv, _, _ := twoBindingHarness(t, wbHandler, healthyCompatHandler(&compatHits), 120*time.Millisecond)

	status, content := postGLM53(t, srv)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if content != "ab" {
		t.Fatalf("content = %q, want full generation \"ab\" from the primary", content)
	}
	if got := wbHits.Load(); got != 1 {
		t.Fatalf("workbuddy hits = %d, want 1", got)
	}
	if got := compatHits.Load(); got != 0 {
		t.Fatalf("compat hits = %d, want 0 (generation must not be clipped into a fallback)", got)
	}
}
