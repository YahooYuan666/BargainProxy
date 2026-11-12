package antigravity

// BP-314: the host fallback must not launder a capacity wall into a quota
// answer.
//
// doStream retries the fallback host on 5xx/429 because the two hosts keep
// SEPARATE quota ledgers (BP-259, measured 2026-11-01: daily 200 in 2.5s while
// prod 429 in 0.8s for the same body). That reasoning is about QUOTA, and it
// does not transfer to a MODEL_CAPACITY_EXHAUSTED 503: Google has no server for
// that model at all, so the answer is identical on both hosts — but prod's own
// ledger has been drained for months, so the fallback replies 429
// RESOURCE_EXHAUSTED within ~300ms.
//
// Field 2026-11-12: the driver asked daily, got "No capacity available for
// model gemini-3.8-flash-high", fell over to prod, and returned prod's
// "Resource has been exhausted (e.g. check quota)" — which the user reasonably
// read as "no quota left", even though the panel showed five_hour at 100%.
// The upstream's own, more precise diagnosis was thrown away.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
)

func TestCapacityWallDoesNotFallOverToTheOtherHost(t *testing.T) {
	var prodHits int
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, capacityBody)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer prod.Close()

	d := New(config.AntigravitySettings{Enabled: true, DailyURL: daily.URL, BaseURL: prod.URL}, &http.Client{})
	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	defer resp.Body.Close()

	if prodHits != 0 {
		t.Fatalf("prod host was asked %d time(s); a fleet-capacity 503 is not the other host's business, "+
			"and prod's drained ledger turns the honest answer into a fake quota wall", prodHits)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the primary's 503 (no capacity)", resp.StatusCode)
	}
	// The body must still be the capacity one, so the classifier can name it.
	raw := make([]byte, 8192)
	n, _ := resp.Body.Read(raw)
	e := classifyHTTP(resp.StatusCode, raw[:n], "gemini-3.8-flash-high")
	if e.Status == 429 || strings.Contains(strings.ToLower(e.Message), "check quota") {
		t.Fatalf("classified message = %q; the capacity diagnosis was replaced by a quota one", e.Message)
	}
}

// Control: the ordinary 5xx fallback (BP-259) must keep working. A plain 500
// with no capacity wording is still worth trying on the other host.
func TestPlain5xxStillFallsOver(t *testing.T) {
	var prodHits int
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":500,"message":"internal"}}`)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodHits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n")
	}))
	defer prod.Close()

	d := New(config.AntigravitySettings{Enabled: true, DailyURL: daily.URL, BaseURL: prod.URL}, &http.Client{})
	resp, err := d.doStream(context.Background(), "token", "m", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	defer resp.Body.Close()
	if prodHits == 0 || resp.StatusCode != http.StatusOK {
		t.Fatalf("plain 5xx must still fall over (prod hits=%d status=%d)", prodHits, resp.StatusCode)
	}
}
