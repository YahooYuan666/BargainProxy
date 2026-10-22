package trae

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

	"bargainproxy/internal/model"
)

// TRAE-6 red-first tests (BP-98): the refresh gate used to skew 24h against a
// ~24h JWT TTL and a 60-min refresh task, so EVERY tick re-exchanged the
// token (24 upstream rotations per account per day, each one spending a
// refresh-token generation). The skew must be tightened, and concurrent
// callers must share one exchange instead of racing the one-time refresh
// token (the loser used to get 20101 and could disable the account).

// TestRefreshSkewTightened: the skew must be small enough that a 24h-TTL JWT
// is NOT re-exchanged on every hourly tick — anything ≤2h passes; 24h fails.
func TestRefreshSkewTightened(t *testing.T) {
	if traeRefreshSkew > 2*time.Hour {
		t.Fatalf("traeRefreshSkew = %s; a 24h TTL with this skew re-exchanges on every hourly tick", traeRefreshSkew)
	}
}

// TestRefreshConcurrentSingleFlight: N concurrent Refresh calls on the same
// account with a due refresh must produce exactly ONE upstream exchange —
// the one-time refresh token must not be raced.
func TestRefreshConcurrentSingleFlight(t *testing.T) {
	var exchanges atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		time.Sleep(30 * time.Millisecond) // widen the race window
		fmt.Fprintf(w, `{"Result":{"Token":"jwt-%d","TokenExpireAt":%d,"RefreshToken":"rt-next"}}`,
			exchanges.Load(), time.Now().Add(48*time.Hour).UnixMilli())
	}))
	defer oauth.Close()
	oldHost := traeOAuthHost
	traeOAuthHost = oauth.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt-old", "refresh_token": "rt-1", "expires_at": time.Now().Add(-time.Hour).Unix()})
	acc := &model.Account{ID: "trae:u-conc", CredsJSON: creds}
	d := testDriver("http://127.0.0.1:1", "http://127.0.0.1:1")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = d.Refresh(context.Background(), acc)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("concurrent refreshes hit upstream %d times, want exactly 1 (one-time refresh token must not be raced)", got)
	}
}

// TestRefreshStillFreshSkipsExchange: a JWT with plenty of runway must not
// be exchanged at all (the cheap path stays cheap).
func TestRefreshStillFreshSkipsExchange(t *testing.T) {
	var exchanges atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		fmt.Fprint(w, `{"Result":{"Token":"jwt-x"}}`)
	}))
	defer oauth.Close()
	oldHost := traeOAuthHost
	traeOAuthHost = oauth.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt-old", "refresh_token": "rt-1", "expires_at": time.Now().Add(23 * time.Hour).Unix()})
	acc := &model.Account{ID: "trae:u-fresh", CredsJSON: creds}
	d := testDriver("http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := exchanges.Load(); got != 0 {
		t.Fatalf("fresh JWT must skip the exchange, got %d", got)
	}
}
