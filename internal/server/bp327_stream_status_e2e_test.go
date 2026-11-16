// BP-327: a streaming request that fails BEFORE any byte reaches the client
// must answer with its honest status, not "200 + an error frame".
//
// The old shape committed 200 with w.WriteHeader(http.StatusOK) before
// gateway.Execute even started, so every terminal refusal that resolves in
// tens of milliseconds — no account for the model (429), quota wall (429),
// concurrency limit (429), a dead binding (502) — reached the client as a
// SUCCESSFUL response whose body happened to carry an error object. Clients
// treat 200 as "the turn ran", so they neither retry nor switch channels.
//
// Field evidence for which cases this actually affects: over the 14 days
// before the fix, the streamed failures that carried a terminal status were
// concurrency_exceeded (0~28ms), quota_exhausted (8~20ms) — i.e. exactly the
// class that used to be masked. (in this test the runtime is memory, but the
// assertion is about the status code, which is what the client reads.)
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
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// noAccountServer builds a server whose only account is model-cooled, so the
// pool refuses before any upstream call. stream=true is the case that used to
// be masked by the early 200.
func noAccountServer(t *testing.T) *httptest.Server {
	t.Helper()
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	t.Cleanup(func() {
		up.Close()
		if hits != 0 {
			t.Errorf("upstream must never be reached, got %d hits", hits)
		}
	})

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
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("wb-stream"), "uid": "u"})
	acc := &model.Account{ID: "wb-s1", Channel: model.ChannelWorkBuddy, CredsJSON: creds}
	p.Upsert(acc)
	p.Lock()
	acc.ModelCooldowns = map[string]model.ModelCooldown{
		"glm-5.3": {Until: time.Now().Add(30 * time.Minute), Reason: "6004 model rate limit"},
	}
	p.Unlock()

	drivers := map[string]driver.Driver{model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// OpenAI chat ingress: stream=true must still answer a terminal status when
// nothing was written.
func TestBP327StreamFailureKeepsHonestStatusOpenAI(t *testing.T) {
	srv := noAccountServer(t)
	body := `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		ct := resp.Header.Get("Content-Type")
		t.Fatalf("stream failure answered 200 (content-type=%q): the client reads that as a successful turn "+
			"and never retries — BP-327 is about committing the status only when a byte is written", ct)
	}
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a terminal >=400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("a pre-byte failure must answer JSON, not an SSE stream: content-type=%q", ct)
	}
}

// Anthropic ingress: same contract on /v1/messages.
func TestBP327StreamFailureKeepsHonestStatusAnthropic(t *testing.T) {
	srv := noAccountServer(t)
	body := `{"model":"glm-5.3","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("anthropic stream failure answered 200 — the client cannot tell the turn failed")
	}
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want >=400", resp.StatusCode)
	}
}
