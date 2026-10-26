package qoder

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
	"bargainproxy/internal/model"
)

// Qoder-2 red-first tests (BP-98): the campaigns endpoints (the check-in the
// 0.2.30 client actually makes) must carry the account's device identity —
// Cosy-MachineToken / Cosy-MachineType (magpie-community/plugins
// packages/qoder 0.2.30 campaignCall + campaignMachine: without the identity
// the list answers 200 with NO CLAIM_BENEFIT campaign, i.e. the daily
// credits silently disappear; Qoder CN moved the daily 100 to the desktop
// benefit center and requires the identity there).
//
// The old daily-check-in endpoints stay as the first path; when they stop
// giving a claimable result the campaigns path takes over.

// TestCheckinCampaignsFallback: a status endpoint that answers something
// other than CLAIMABLE/DISABLED/CLAIMED (the new shape: the old endpoint
// is gone or no longer lists the daily credit) must fall back to the
// campaigns list and claim the CLAIM_BENEFIT campaign.
func TestCheckinCampaignsFallback(t *testing.T) {
	var campaignHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			// The old endpoint no longer knows the daily credit:
			// an unknown/empty status must trigger the fallback.
			fmt.Fprint(w, `{"status":"UNKNOWN"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			campaignHits++
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]}`)
		case strings.Contains(r.URL.Path, "/campaigns/c1/claim"):
			campaignHits++
			// The CAMPAIGNS claim shape (BP-124, magpie native-checkin): status CLAIMED
			// with the reward nested in benefit.amount — NOT the legacy
			// {"success":true,"rewardCredits":N} of the retired check-in endpoint.
			fmt.Fprint(w, `{"status":"CLAIMED","benefit":{"amount":100,"kind":"CREDITS"}}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	cfg := config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}
	d := New(cfg, nil)
	acc := testQoderAccount(t)
	checkedIn, credits, enable, err := d.Checkin(context.Background(), acc)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !checkedIn || credits != 100 || !enable {
		t.Fatalf("campaigns claim result = %v/%v/%v, want true/100/true", checkedIn, credits, enable)
	}
	if campaignHits < 2 {
		t.Fatalf("campaigns list+claim not both called: %d", campaignHits)
	}
}

// TestCheckinCampaignsCarriesMachineIdentity: the campaigns requests must
// carry Cosy-MachineToken / Cosy-MachineType (and a client-like UA), not
// "Go-http-client/2.0" with no identity — that is what 0.2.30 fixed.
func TestCheckinCampaignsCarriesMachineIdentity(t *testing.T) {
	var sawToken, sawType, sawUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"UNKNOWN"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			sawToken = r.Header.Get("Cosy-MachineToken")
			sawType = r.Header.Get("Cosy-MachineType")
			sawUA = r.Header.Get("User-Agent")
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":100}}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	cfg := config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}
	d := New(cfg, nil)
	acc := testQoderAccount(t)
	if _, _, _, err := d.Checkin(context.Background(), acc); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if sawToken == "" {
		t.Fatal("campaigns request must carry Cosy-MachineToken (else the list omits CLAIM_BENEFIT)")
	}
	if sawType == "" {
		t.Fatal("campaigns request must carry Cosy-MachineType")
	}
	if sawUA == "Go-http-client/2.0" {
		t.Fatalf("campaigns UA must read as the client, got %q", sawUA)
	}
}

// TestCheckinCampaignsAlreadyClaimed: a CLAIMED campaign reads as the day's
// done, not a failure.
func TestCheckinCampaignsAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"UNKNOWN"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":100}}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	cfg := config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}
	d := New(cfg, nil)
	checkedIn, _, _, err := d.Checkin(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("claimed campaign must not error: %v", err)
	}
	if !checkedIn {
		t.Fatal("CLAIMED must read as checked-in")
	}
}

// testQoderAccount builds a minimal account blob; the exchange endpoint is
// not hit because the check-in path takes the openapi bearer, stubbed here
// via the same server (pathJobExchange answers a minimal envelope).
func testQoderAccount(t *testing.T) *model.Account {
	t.Helper()
	creds, _ := json.Marshal(map[string]any{"pat": "pt-test", "name": "t"})
	return &model.Account{ID: "qoder:t", Channel: model.ChannelQoder, CredsJSON: creds}
}
