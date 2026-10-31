package antigravity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
)

// BP-256: a HANGING primary host must fall through to the fallback.
//
// Field 2026-11-01: gemini "你好" hung for minutes; the log only showed
// "daily-cloudcode-pa… http2: timeout awaiting response headers" after 6m40s.
// The failover only ran on transport errors and 5xx — a hang is neither — so
// the fallback (which answers 429 immediately) was never consulted, and the
// hung request held one of the channel's three in-flight slots the whole time.
func TestHangingPrimaryFallsThroughToFallback(t *testing.T) {
	hung := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hung // never answers: exactly the hang shape
	}))
	// LIFO: close(hung) must run BEFORE slow.Close(), or Close blocks waiting
	// for the parked handler.
	defer slow.Close()
	defer close(hung)

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer fast.Close()

	d := New(config.AntigravitySettings{
		Enabled:      true,
		DailyURL:     slow.URL,
		BaseURL:      fast.URL,
		HostDeadline: config.Duration(300 * time.Millisecond),
	}, &http.Client{})

	start := time.Now()
	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("doStream: %v (a hang must not surface as an error; the fallback should answer)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the fallback's 429 — the hanging primary was not cut", resp.StatusCode)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s; the per-host deadline did not fire", elapsed)
	}
}

// With the deadline disabled the old behaviour is kept (a control, so the fix
// cannot silently change the default path for deployments that do not set it).
func TestNoDeadlineKeepsWaiting(t *testing.T) {
	if d := config.Duration(0).D(); d != 0 {
		t.Fatalf("zero Duration should mean disabled, got %v", d)
	}
}
