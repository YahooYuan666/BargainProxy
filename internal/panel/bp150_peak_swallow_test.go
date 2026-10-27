package panel_test

// BP-150 repro: does clicking 刷新额度 swallow the AutoClaw high-water mark?
// This goes through the REAL entry (the panel's accounts/probe handler →
// gateway.ApplyProbe → pool.RecordCredits), not the helper in isolation —
// testing only the helper is how a bug hides behind a green unit test.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/panel"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// fakeAutoClaw serves the one endpoint Probe reads (wallets) plus the expiring
// one, with a balance the test can change between clicks.
func fakeAutoClaw(t *testing.T, balance *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v2/wallets"):
			fmt.Fprintf(w, `{"data":{"total_balance":%d}}`, balance.Load())
		case strings.Contains(r.URL.Path, "points/expiring"):
			fmt.Fprint(w, `{"data":{"expiring_points":0}}`)
		default:
			// Unknown paths must not be mistaken for a balance.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{}`)
		}
	}))
}

// probeOnce drives the panel's real refresh handler for one account, through
// HTTP with the process's open-mode CSRF token (the same path the browser
// takes).
func probeOnce(t *testing.T, srvURL, csrf, id string) {
	t.Helper()
	resp := postJSON(t, srvURL, "/panel/api/accounts/probe", csrf, map[string]string{"id": id})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("probe returned %d: %s", resp.StatusCode, b)
	}
}

// openCSRF fetches the token the panel hands an open-mode session.
func openCSRF(t *testing.T, srvURL string) string {
	t.Helper()
	resp, err := http.Get(srvURL + "/panel/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	var st struct {
		CSRF string `json:"csrf"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.CSRF == "" {
		t.Fatal("state returned no csrf token")
	}
	return st.CSRF
}

// TestAutoClawRefreshKeepsHistoricalPeak is the user's report: the account
// once held 13000 points, spent down to 12673, and pressing 刷新额度 made the
// panel read "12673/12673" — the 13000 high-water mark gone.
func TestAutoClawRefreshKeepsHistoricalPeak(t *testing.T) {
	var balance atomic.Int64
	balance.Store(13000)
	up := fakeAutoClaw(t, &balance)
	defer up.Close()

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.AutoClaw.Enabled = true
	cfg.Channels.AutoClaw.UserAPIBase = up.URL
	cfg.Channels.AutoClaw.OverseasBase = up.URL
	cfg.Channels.AutoClaw.AppSecret = "test-secret"

	st := store.NewMemory()
	pl := pool.New(pool.Params{MaxQueueWait: time.Second}, nil, nil)
	drivers := map[string]driver.Driver{
		model.ChannelAutoClaw: autoclaw.New(cfg.Channels.AutoClaw, nil),
	}
	gw := gateway.New(cfg, pl, router.New(cfg), drivers, st)

	creds, _ := json.Marshal(map[string]any{"access_token": "tok", "uid": "u-1", "region": "cn"})
	acc := &model.Account{ID: "autoclaw:u-1", Channel: model.ChannelAutoClaw, UID: "u-1", Nickname: "acct", CredsJSON: creds}
	pl.Upsert(acc)
	_ = st.SaveAccount(acc)

	pn := panel.New(cfg, gw, st, nil)
	mux := http.NewServeMux()
	pn.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	csrf := openCSRF(t, srv.URL)

	// 1st click at 13000: the peak becomes 13000.
	probeOnce(t, srv.URL, csrf, acc.ID)
	if acc.Credits.Peak != 13000 {
		t.Fatalf("after the first refresh peak = %d, want 13000", acc.Credits.Peak)
	}

	// Spend down upstream, then click again.
	balance.Store(12673)
	probeOnce(t, srv.URL, csrf, acc.ID)
	if acc.Credits.Remain != 12673 {
		t.Fatalf("remain = %d, want 12673", acc.Credits.Remain)
	}
	if acc.Credits.Peak != 13000 {
		t.Fatalf("刷新额度 swallowed the peak: peak = %d, want the historical 13000", acc.Credits.Peak)
	}

	// A third click with the balance unchanged must not move it either.
	probeOnce(t, srv.URL, csrf, acc.ID)
	if acc.Credits.Peak != 13000 {
		t.Fatalf("third refresh: peak = %d, want 13000", acc.Credits.Peak)
	}
}
