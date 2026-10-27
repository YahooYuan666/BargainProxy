package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// BP-128: a 503 upstream-busy carries Retry-After too (qoderhub #99 — an
// aggregator without the header retries immediately and deepens the queue),
// and the value is capped at 600s so a multi-hour model cooldown cannot hang
// a CLI that sleeps Retry-After*1000 verbatim (misakano 436631c).
func TestRetryAfter503AndCap(t *testing.T) {
	// 503 with a known reset: header present.
	w := httptest.NewRecorder()
	err := uperr.New(uperr.ClassUpstreamBusy, 503, "queue")
	err.ResetAt = time.Now().Add(90 * time.Second)
	withRetryAfter(w, http.StatusServiceUnavailable, err)
	if got := w.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("503 Retry-After = %q, want 90", got)
	}

	// 429 with a multi-hour reset: capped at 600.
	w2 := httptest.NewRecorder()
	err2 := uperr.New(uperr.ClassModelRate, 429, "model hold")
	err2.ResetAt = time.Now().Add(5 * time.Hour)
	withRetryAfter(w2, http.StatusTooManyRequests, err2)
	if got := w2.Header().Get("Retry-After"); got != "600" {
		t.Fatalf("429 Retry-After = %q, want 600 (cap)", got)
	}

	// Other statuses stay bare.
	w3 := httptest.NewRecorder()
	withRetryAfter(w3, http.StatusInternalServerError, err)
	if got := w3.Header().Get("Retry-After"); got != "" {
		t.Fatalf("500 Retry-After = %q, want empty", got)
	}
}
