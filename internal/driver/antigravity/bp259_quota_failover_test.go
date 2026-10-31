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

// BP-259: the two hosts keep SEPARATE quota ledgers, so a 429 on one is not
// the account's verdict.
//
// Measured 2026-11-01 03:10 with ONE 180KB / 45k-token body against all three
// accounts:
//
//	daily-cloudcode-pa: 200 in 2.5s / 3.4s / 19.7s
//	cloudcode-pa     : 429 RESOURCE_EXHAUSTED in 0.7s / 0.8s / 0.9s
//
// Same token, same body, opposite verdicts. The driver failed over only on
// transport errors and 5xx, so a 429 on the primary ended the request even
// though the other host had headroom — which is exactly the reported shape:
// the Antigravity IDE (daily) answers in seconds while the gateway says
// "upstream rate limited".
func TestQuotaWallOnPrimaryTriesTheOtherHost(t *testing.T) {
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer daily.Close()

	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n")
	}))
	defer prod.Close()

	d := New(config.AntigravitySettings{
		Enabled:  true,
		DailyURL: daily.URL,
		BaseURL:  prod.URL,
	}, &http.Client{})

	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a 429 on the daily host must fall through to prod, "+
			"which has its own quota ledger", resp.StatusCode)
	}
}

// When BOTH hosts are out of quota the answer stays a 429 (model-scoped
// cooldown must still apply) — the failover must not turn a quota wall into a
// transport error.
func TestBothHostsOutOfQuotaStays429(t *testing.T) {
	mk := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`)
		}))
	}
	daily := mk()
	defer daily.Close()
	prod := mk()
	defer prod.Close()

	d := New(config.AntigravitySettings{Enabled: true, DailyURL: daily.URL, BaseURL: prod.URL}, &http.Client{})
	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 so the pool applies a model-scoped cooldown", resp.StatusCode)
	}
}

// A real 200 on the primary must never be disturbed by the failover logic.
func TestPrimarySuccessNotRetried(t *testing.T) {
	var prodHits int
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {}\n\n")
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer prod.Close()

	d := New(config.AntigravitySettings{Enabled: true, DailyURL: daily.URL, BaseURL: prod.URL}, &http.Client{})
	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	resp.Body.Close()
	if prodHits != 0 {
		t.Fatalf("prod was hit %d times for a healthy primary", prodHits)
	}
}

// A 400 (bad payload) must NOT fail over: the same body would fail on both
// hosts, and retrying doubles the latency of a caller mistake.
func TestBadRequestDoesNotFailOver(t *testing.T) {
	var prodHits int
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"Request contains an invalid argument."}}`)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer prod.Close()

	d := New(config.AntigravitySettings{Enabled: true, DailyURL: daily.URL, BaseURL: prod.URL}, &http.Client{})
	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	resp.Body.Close()
	if prodHits != 0 {
		t.Fatalf("a 400 was retried on the fallback host (%d hits); it would fail there too", prodHits)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the primary's 400", resp.StatusCode)
	}
}

var _ = time.Second
