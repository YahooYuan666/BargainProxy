package panel_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
)

// TestWBFlowNotKilledWhilePolling covers the 2026-10-22 field failure: the
// five-minute expiry timer fired blindly at start+5m even though the user was
// still actively polling (they just needed longer in the browser for account
// #2), marking the flow failed while the upstream had already issued the
// token. The timer must honour the sliding expiry instead.
func TestWBFlowNotKilledWhilePolling(t *testing.T) {
	wbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"state":"st-exp","authUrl":"https://www.workbuddy.ai/login?state=st-exp"}}`)
		case "/v2/plugin/auth/token":
			fmt.Fprint(w, `{"code":11217,"msg":"11217:login ing...","data":{}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer wbSrv.Close()

	srv, panel := buildPanelServerWith(t, func(cfg *config.Config) {
		cfg.Channels.WorkBuddy.ChatBaseCN = wbSrv.URL
		cfg.Channels.WorkBuddy.ChatBaseGL = wbSrv.URL
		cfg.WorkBuddyGlobal = true
	})
	cookie, csrf := loginHelper(t, srv)

	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/workbuddy/start", cookie, csrf, `{"realm":"global"}`)
	var start struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()

	key := "w:" + start.State

	// Simulate an actively-polled flow whose sliding expiry is still ahead
	// (touchFlow moved it) but whose original 5-minute window elapsed.
	panel.FlowLock()
	panel.SetFlowExpiryForTest(key, time.Now().Add(2*time.Minute)) // slid by a poll
	panel.FlowUnlock()

	panel.ExpireFlowForTest(key) // the old code failed the flow HERE
	if st := panel.FlowStatus(key); st != "pending" {
		t.Fatalf("flow status = %q, want pending (expiry had not passed)", st)
	}

	// Now the sliding expiry genuinely passes -> the flow must fail.
	panel.FlowLock()
	panel.SetFlowExpiryForTest(key, time.Now().Add(-time.Second))
	panel.FlowUnlock()
	panel.ExpireFlowForTest(key)
	if st := panel.FlowStatus(key); st != "failed" {
		t.Fatalf("flow status = %q, want failed (expiry passed)", st)
	}
}
