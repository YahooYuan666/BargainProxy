package server_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"bargainproxy/internal/uperr"
)

// testToken assembles placeholder tokens at runtime (no credential-shaped
// literals in the source tree).
func testToken(kind string) string {
	return strings.Join([]string{"tok", kind}, "-")
}

func upstreamAuthErr() *uperr.Error {
	return uperr.New(uperr.ClassAuth, 401, "scripted auth failure")
}

// buildCrossChannelServer binds glm-5.3 to Trae (priority 0) and WorkBuddy
// (priority 1). Both channels get one account and their own mock upstream.
func buildCrossChannelServer(t *testing.T, traeUp, wbUp *httptest.Server) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FallbackWait = config.Duration(300 * time.Millisecond) // fast cascade in tests
	cfg.Channels.Trae.AgentHost = traeUp.URL
	cfg.Channels.Trae.UgHost = traeUp.URL
	cfg.Channels.WorkBuddy.ChatBaseCN = wbUp.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = wbUp.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelTrae, UpstreamModel: "glm-5.2", Priority: 0},
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelTrae:      1, // serial
		model.ChannelWorkBuddy: 2,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })

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
	return srv, p
}

func sseOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	fmt.Fprintln(w, `data: [DONE]`)
}

func TestCrossChannelPrefersTraeThenFallsToWorkBuddy(t *testing.T) {
	var traeHits, wbHits int
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traeHits++
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		sseOK(w)
	}))
	defer wbUp.Close()

	srv, p := buildCrossChannelServer(t, traeUp, wbUp)
	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`

	// 1) Trae healthy: request must go to Trae.
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || traeHits != 1 || wbHits != 0 {
		t.Fatalf("first request: status=%d traeHits=%d wbHits=%d", resp.StatusCode, traeHits, wbHits)
	}

	// 2) Occupy the Trae account (serial limit=1) by holding a lease.
	lease, err := p.Acquire(context.Background(), model.ChannelTrae, "glm-5.3", "")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}

	// 3) Next request must cascade to WorkBuddy after the short fallback wait.
	start := time.Now()
	resp2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp2.Body.Close()
	elapsed := time.Since(start)
	lease.Release()
	if resp2.StatusCode != 200 {
		t.Fatalf("second request status %d", resp2.StatusCode)
	}
	if wbHits != 1 {
		t.Fatalf("expected workbuddy to serve the second request, wbHits=%d", wbHits)
	}
	if resp2.Header.Get("X-BP-Served-Channel") != model.ChannelWorkBuddy {
		t.Fatalf("degraded header missing: %q", resp2.Header.Get("X-BP-Served-Channel"))
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cascade took too long: %s", elapsed)
	}
}

func TestCrossChannelTraeCoolingFallsThrough(t *testing.T) {
	var traeHits, wbHits int
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traeHits++
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		sseOK(w)
	}))
	defer wbUp.Close()

	srv, p := buildCrossChannelServer(t, traeUp, wbUp)

	// Cool the Trae account at the account level (10-minute grace via auth).
	acc := p.Accounts(model.ChannelTrae)[0]
	p.ApplyResult(acc, "glm-5.3", upstreamAuthErr())

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if traeHits != 0 {
		t.Fatalf("cooled trae must not be hit, got %d", traeHits)
	}
	if wbHits != 1 {
		t.Fatalf("workbuddy should serve, got %d", wbHits)
	}
}

// TestChannelPrefixEscapeHatchE2E: "channel:<name>:<model>" pins the request
// to one channel even when a preferred sibling exists, echoes the bare model
// name, and an unknown prefix channel keeps plain 404 semantics.
func TestChannelPrefixEscapeHatchE2E(t *testing.T) {
	var traeHits, wbHits int
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traeHits++
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		sseOK(w)
	}))
	defer wbUp.Close()

	srv, _ := buildCrossChannelServer(t, traeUp, wbUp)

	// Trae is priority 0; the escape hatch must still land on workbuddy.
	body := `{"model":"channel:workbuddy:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("forced-channel status = %d", resp.StatusCode)
	}
	if wbHits != 1 || traeHits != 0 {
		t.Fatalf("escape hatch must pin to workbuddy: traeHits=%d wbHits=%d", traeHits, wbHits)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Model != "glm-5.3" {
		t.Fatalf("echoed model = %q, want the bare name", out.Model)
	}

	// Unknown prefix channel: 404 semantics, no upstream traffic.
	badBody := `{"model":"channel:bogus:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	bad, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(badBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != 404 {
		t.Fatalf("unknown prefix channel status = %d, want 404", bad.StatusCode)
	}
	if traeHits+wbHits != 1 {
		t.Fatalf("404 must not hit upstreams: traeHits=%d wbHits=%d", traeHits, wbHits)
	}
}

// TestEmptyPoolBindingSkippedWithoutFallbackWait: a route row whose channel
// has no imported account must be skipped outright — no fallback wait, no
// misleading upstream attempt — instead of holding each request for
// FallbackWait before cascading (the AgentPoints "silent reroute + 8s stall"
// lesson). A pinned channel: escape hatch keeps its binding even when empty
// (explicit intent beats convenience: fast ErrNoCapacity, not a reroute).
func TestEmptyPoolBindingSkippedWithoutFallbackWait(t *testing.T) {
	var wbHits int
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("empty-pool trae binding must never be attempted")
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		sseOK(w)
	}))
	defer wbUp.Close()

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FallbackWait = config.Duration(2 * time.Second) // long enough to fail the test if ever waited on
	cfg.Channels.Trae.AgentHost = traeUp.URL
	cfg.Channels.Trae.UgHost = traeUp.URL
	cfg.Channels.WorkBuddy.ChatBaseCN = wbUp.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = wbUp.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelTrae, UpstreamModel: "glm-5.2", Priority: 0},
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelTrae:      1,
		model.ChannelWorkBuddy: 2,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	// Only the workbuddy account exists: the trae binding is empty-pool.
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

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != 200 || wbHits != 1 {
		t.Fatalf("empty trae binding must be skipped: status=%d wbHits=%d", resp.StatusCode, wbHits)
	}
	if elapsed >= cfg.FallbackWait.D() {
		t.Fatalf("request waited %s (>= fallback wait %s): empty binding was not skipped", elapsed, cfg.FallbackWait.D())
	}

	// Escape hatch pinning the EMPTY channel: fast quota-exhausted failure,
	// never a silent reroute to workbuddy.
	pinned := `{"model":"channel:trae:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	pStart := time.Now()
	pResp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(pinned))
	if err != nil {
		t.Fatalf("pinned post: %v", err)
	}
	pResp.Body.Close()
	pElapsed := time.Since(pStart)
	if pResp.StatusCode != 429 {
		t.Fatalf("pinned empty-channel status = %d, want 429 fast-fail", pResp.StatusCode)
	}
	if pElapsed >= cfg.FallbackWait.D() {
		t.Fatalf("pinned empty channel waited %s: must fail fast via ErrNoCapacity", pElapsed)
	}
	if wbHits != 1 {
		t.Fatalf("pinned empty channel must not reroute to workbuddy: wbHits=%d", wbHits)
	}

	// Escape hatch pinning the HEALTHY channel still serves.
	okBody := `{"model":"channel:workbuddy:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	okResp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(okBody))
	if err != nil {
		t.Fatalf("pinned ok post: %v", err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != 200 || wbHits != 2 {
		t.Fatalf("pinned healthy channel must serve: status=%d wbHits=%d", okResp.StatusCode, wbHits)
	}
}

// TestQueueFullUpstreamBusyNoRotation: a CodeBuddy queue-full (429 + 6021)
// from the first binding must NOT rotate into the next account/binding —
// rotating ACROSS accounts against one full queue is the miskill failure
// mode. BP-169 (user option A, misakano f07eba54): the SAME lease is retried
// in-request with a short backoff (3 upstream calls total — queue signals
// are intermittent, two accounts recovered within 33s), and the terminal is
// 429 + Retry-After (the WorkBuddy CLI auto-resends on 429 and ABORTS on
// 503). The sibling channel sees zero traffic, the account state stays
// clean, and the channel+model pause makes the next request fail fast.
func TestQueueFullUpstreamBusyNoRotation(t *testing.T) {
	var traeHits, wbHits int
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traeHits++
		sseOK(w)
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		// CodeBuddy queue-full: HTTP 429 + business code 6021.
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":6021,"msg":"queue.full.title","requestId":"q-1"}`)
	}))
	defer wbUp.Close()

	srv, p := buildCrossChannelServer(t, traeUp, wbUp)
	// Pin workbuddy: it is the lower-priority binding here, and pinning keeps
	// trae out of the picture entirely (the rotation we are proving absent is
	// workbuddy's own cross-account rotation plus any cross-binding cascade).
	body := `{"model":"channel:workbuddy:glm-5.3","messages":[{"role":"user","content":"hi"}]}`

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	retryAfter := resp.Header.Get("Retry-After")
	var errBody struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("queue-full must surface 429 with Retry-After, got %d (%+v)", resp.StatusCode, errBody)
	}
	if errBody.Error.Code != "upstream_busy" {
		t.Fatalf("error code = %q, want upstream_busy", errBody.Error.Code)
	}
	if retryAfter == "" {
		t.Fatal("429 must carry Retry-After (the CLI auto-resends on it)")
	}
	// Same-lease backoff retries, zero cross-account/cross-channel rotation.
	if wbHits != 3 || traeHits != 0 {
		t.Fatalf("queue-full must retry only the same lease: wbHits=%d (want 3) traeHits=%d (want 0)", wbHits, traeHits)
	}

	// Account state machine untouched (the miskill axis).
	for _, acc := range p.Accounts(model.ChannelWorkBuddy) {
		if !acc.AccountCooldownUntil.IsZero() || len(acc.ModelCooldowns) != 0 || acc.ConsecutiveFails != 0 {
			t.Fatalf("queue-full must not penalize the account: %+v", acc)
		}
	}

	// The channel+model pause makes the immediate retry fail fast — no
	// further upstream traffic on either channel. The pinned form shares the
	// pause because it resolves to the same channel+model pair.
	resp2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("retry post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 429 || wbHits != 3 || traeHits != 0 {
		t.Fatalf("paused retry must fail fast 429 without upstream hits: status=%d wbHits=%d traeHits=%d", resp2.StatusCode, wbHits, traeHits)
	}

	// A healthy pinned channel serves normally while workbuddy is paused.
	pinned := `{"model":"channel:trae:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	pResp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(pinned))
	if err != nil {
		t.Fatalf("pinned post: %v", err)
	}
	pResp.Body.Close()
	if pResp.StatusCode != 200 || traeHits != 1 {
		t.Fatalf("pinned healthy channel must serve during wb pause: status=%d traeHits=%d", pResp.StatusCode, traeHits)
	}
}
