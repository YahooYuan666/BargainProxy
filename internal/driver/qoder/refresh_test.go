package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// Qoder-5 red-first tests (BP-98): the job token must be RENEWED through
// /api/v1/jobToken/refresh with the stored refresh token (magpie
// community/plugins packages/qoder 0.2.30 refreshJob) instead of
// re-exchanging the PAT on every expiry — a PAT exchange is a fresh sign-in
// and reads as one to upstream risk control, and two concurrent callers
// discovering the same expiry used to exchange twice.

// buildRefreshHarness spins a server speaking enough of the protocol:
// gateway jobToken exchange, openapi bearer exchange, and (optionally)
// jobToken refresh. Counters record which endpoint served how often.
type refreshHarness struct {
	srv         *httptest.Server
	exchange    atomic.Int32
	refresh     atomic.Int32
	refreshFail atomic.Bool
}

func newRefreshHarness(t *testing.T) *refreshHarness {
	t.Helper()
	h := &refreshHarness{}
	mux := http.NewServeMux()
	mux.HandleFunc("/algo/api/v3/user/jobToken", func(w http.ResponseWriter, r *http.Request) {
		h.exchange.Add(1)
		fmt.Fprintf(w, `{"id":"u-1","name":"T","userType":"personal_standard","securityOauthToken":"sec","refreshToken":"rt-%d","expireTime":%d}`,
			h.exchange.Load(), time.Now().Add(90*time.Minute).UnixMilli())
	})
	mux.HandleFunc("/api/v1/jobToken/refresh", func(w http.ResponseWriter, r *http.Request) {
		if h.refreshFail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"refresh token invalid"}`)
			return
		}
		h.refresh.Add(1)
		fmt.Fprintf(w, `{"token":"job-%d","refresh_token":"rt-next-%d","expires_in":7200}`, h.refresh.Load(), h.refresh.Load())
	})
	mux.HandleFunc("/api/v1/jobToken/exchange", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"token":"open-bearer","expires_in":7200}`)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func refreshAccount() *model.Account {
	creds, _ := json.Marshal(map[string]any{"pat": "pt-test", "name": "t"})
	return &model.Account{ID: "qoder:rt", Channel: model.ChannelQoder, CredsJSON: creds}
}

func refreshDriver(h *refreshHarness) *Driver {
	return New(config.QoderSettings{GatewayBase: h.srv.URL, OpenBase: h.srv.URL}, nil)
}

// TestRefreshPrefersJobTokenRefresh: after the bootstrap exchange, a second
// ensureFreshSession past the expiry must hit the REFRESH endpoint (the
// stored refresh token renewed), not the PAT exchange again.
func TestRefreshPrefersJobTokenRefresh(t *testing.T) {
	h := newRefreshHarness(t)
	d := refreshDriver(h)
	acc := refreshAccount()
	rt, err := d.runtimeFor(acc)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := d.ensureFreshSession(context.Background(), rt); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if h.exchange.Load() != 1 || h.refresh.Load() != 0 {
		t.Fatalf("bootstrap must exchange once: exchange=%d refresh=%d", h.exchange.Load(), h.refresh.Load())
	}
	// Force the session stale, then re-ensure.
	rt.mu.Lock()
	rt.expireTimeMs = time.Now().UnixMilli() - 1000
	rt.mu.Unlock()
	if err := d.ensureFreshSession(context.Background(), rt); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if h.refresh.Load() != 1 {
		t.Fatalf("stale session must refresh via the refresh endpoint, refresh=%d exchange=%d", h.refresh.Load(), h.exchange.Load())
	}
	if h.exchange.Load() != 1 {
		t.Fatalf("renewal must NOT re-exchange the PAT, exchange=%d", h.exchange.Load())
	}
	rt.mu.Lock()
	tok := rt.sess != nil
	rt.mu.Unlock()
	if !tok {
		t.Fatal("session missing after renewal")
	}
}

// TestRefreshConcurrentSingleFlight: N concurrent ensureFreshSession calls
// on a stale session must produce ONE upstream renewal — the refresh token
// is one-time (magpie 0.2.30: "a refresh token is spent once, so two
// refreshes would spend it twice").
func TestRefreshConcurrentSingleFlight(t *testing.T) {
	h := newRefreshHarness(t)
	d := refreshDriver(h)
	acc := refreshAccount()
	rt, err := d.runtimeFor(acc)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := d.ensureFreshSession(context.Background(), rt); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	rt.mu.Lock()
	rt.expireTimeMs = time.Now().UnixMilli() - 1000
	rt.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = d.ensureFreshSession(context.Background(), rt)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if got := h.refresh.Load(); got != 1 {
		t.Fatalf("concurrent renewal hit the refresh endpoint %d times, want 1", got)
	}
}

// TestRefreshFallsBackToExchange: a refresh refused as lapsed (401) falls
// back to the PAT exchange — the PAT lasts a year, the refresh two days
// (magpie 0.2.30 renew()).
func TestRefreshFallsBackToExchange(t *testing.T) {
	h := newRefreshHarness(t)
	d := refreshDriver(h)
	acc := refreshAccount()
	rt, err := d.runtimeFor(acc)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := d.ensureFreshSession(context.Background(), rt); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	h.refreshFail.Store(true)
	rt.mu.Lock()
	rt.expireTimeMs = time.Now().UnixMilli() - 1000
	rt.mu.Unlock()
	if err := d.ensureFreshSession(context.Background(), rt); err != nil {
		t.Fatalf("renew with dead refresh token: %v", err)
	}
	if h.exchange.Load() != 2 {
		t.Fatalf("refresh refusal must fall back to the PAT exchange, exchange=%d refresh=%d", h.exchange.Load(), h.refresh.Load())
	}
}
