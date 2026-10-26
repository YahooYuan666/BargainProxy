package qoder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
)

// BP-120: Qoder CN retired the legacy daily-check-in endpoint. It now answers
// `{"status":"DISABLED"}` for EVERY account, while the daily 100 credits live
// on the campaigns page as an active CLAIM_BENEFIT campaign.
//
// Field 2026-10-26: the task ledger held "qoder checkin not available" every
// day, and the account card showed 0 claimable, while the upstream campaigns
// document (read live) carried
//
//	"title": "每天领 100 Credits", "claimStatus": "CLAIMED",
//	"benefit": {"amount": 100, "kind": "CREDITS"}
//
// i.e. the programme was running and the account was claiming it — just never
// through us. Checkin() returned enable=false on DISABLED and never looked at
// the campaigns page.

// TestCheckinDisabledFallsBackToCampaigns is the red-first test: a DISABLED
// legacy status must still reach the campaigns claim.
func TestCheckinDisabledFallsBackToCampaigns(t *testing.T) {
	var claimed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100,"kind":"CREDITS"}}]}`)
		case strings.Contains(r.URL.Path, "/campaigns/c1/claim"):
			claimed = true
			fmt.Fprint(w, `{"success":true,"rewardCredits":100}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	checkedIn, credits, enable, err := d.Checkin(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !claimed {
		t.Fatal("DISABLED must fall through to the campaigns claim — Qoder CN moved the daily 100 credits there")
	}
	if !checkedIn || credits != 100 || !enable {
		t.Fatalf("result = (%v, %d, %v), want (true, 100, true)", checkedIn, credits, enable)
	}
}

// TestCheckinStatusReadsCampaignsWhenLegacyDisabled: the panel's status read has
// to agree with Checkin, or the account card says "no check-in programme" while
// a claim is sitting there available.
func TestCheckinStatusReadsCampaignsWhenLegacyDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	st, err := d.CheckinStatus(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Disabled {
		t.Fatal("status still says DISABLED while an active CLAIM_BENEFIT campaign is claimable")
	}
	if !st.Claimable {
		t.Fatalf("status = %+v, want claimable from the campaigns page", st)
	}
}

// TestCheckinStatusClaimedCampaignIsClaimed: the same read must report a
// claimed campaign as claimed, not as an inactive programme.
func TestCheckinStatusClaimedCampaignIsClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED","currentStreakDays":0}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":100}}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	st, err := d.CheckinStatus(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Claimed || st.Disabled {
		t.Fatalf("status = %+v, want claimed (not disabled)", st)
	}
}

// TestCheckinStatusInactiveCampaignStaysDisabled: with no active campaign at
// all, DISABLED is the honest answer — the fix must not invent a claim.
func TestCheckinStatusInactiveCampaignStaysDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			// Expired campaign: endAt in the past.
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","endAt":1000,"benefit":{"amount":100}}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	st, err := d.CheckinStatus(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Disabled || st.Claimable {
		t.Fatalf("status = %+v, want disabled (the only campaign is expired)", st)
	}
}

// TestProbeCarriesAddonTotal pins the second Qoder credit pool: the add-on
// package is reported as remain AND its issued size (BP-122). Without the
// total the panel could only say "另有加赠 382", which never shows how much of
// the pack has been used or whether a fresh pack landed.
func TestProbeCarriesAddonTotal(t *testing.T) {
	res := probeCreditsFromDoc(map[string]any{
		"userType": "personal_professional_trial",
		"userQuota": map[string]any{
			"remaining": float64(0), "total": float64(300), "used": float64(300), "unit": "credits",
		},
		"addOnQuota": map[string]any{
			"remaining": float64(382), "total": float64(400), "used": float64(18), "unit": "credits",
		},
	})
	if res.Credits == nil {
		t.Fatal("no credits in probe result")
	}
	if res.Credits.Remain != 0 || res.Credits.Total != 300 {
		t.Fatalf("plan pool = %d/%d, want 0/300", res.Credits.Remain, res.Credits.Total)
	}
	if res.Credits.Bonus != 382 || res.Credits.BonusTotal != 400 {
		t.Fatalf("add-on pool = %d/%d, want 382/400", res.Credits.Bonus, res.Credits.BonusTotal)
	}
}
