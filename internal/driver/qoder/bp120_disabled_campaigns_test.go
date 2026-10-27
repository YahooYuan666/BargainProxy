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
			// The CAMPAIGNS claim shape (BP-124, magpie native-checkin): status CLAIMED
			// with the reward nested in benefit.amount — NOT the legacy
			// {"success":true,"rewardCredits":N} of the retired check-in endpoint.
			fmt.Fprint(w, `{"status":"CLAIMED","benefit":{"amount":100,"kind":"CREDITS"}}`)
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
	// BP-151: landed claims report the reward (false/100/true).
	if checkedIn || credits != 100 || !enable {
		t.Fatalf("result = (%v, %d, %v), want (false, 100, true)", checkedIn, credits, enable)
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

// --- BP-124: the campaigns CLAIM response shape ---

// TestCampaignsClaimReadsNestedBenefit: a successful claim answers
// `status:"CLAIMED"` with the reward in the NESTED benefit.amount. The old
// parsing read the legacy flat keys (`rewardCredits`, `benefit.amount` as one
// literal map key) and reported every successful claim as "+0 credits".
func TestCampaignsClaimReadsNestedBenefit(t *testing.T) {
	var claimHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100,"kind":"CREDITS"}}]}`)
		case strings.Contains(r.URL.Path, "/campaigns/c1/claim"):
			claimHits++
			fmt.Fprint(w, `{"status":"CLAIMED","benefit":{"amount":100,"kind":"CREDITS"}}`)
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
	if claimHits != 1 {
		t.Fatalf("claim called %d times, want 1", claimHits)
	}
	// BP-151: a landed claim reports checkedIn=false + the reward, so the
	// task ledger renders "+100 credits" (and taskRunCredits books it)
	// instead of an uninformative "already checked in today".
	if checkedIn || !enable {
		t.Fatalf("result = (%v, %v), want (false, true)", checkedIn, enable)
	}
	if credits != 100 {
		t.Fatalf("reward = %d, want 100 from the nested benefit.amount", credits)
	}
}

// TestCampaignsClaimDataEnvelope: the same answer wrapped in a data envelope
// must read the same way (magpie checks claim?.status ?? claim?.data?.status).
func TestCampaignsClaimDataEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]}`)
		case strings.Contains(r.URL.Path, "/campaigns/c1/claim"):
			fmt.Fprint(w, `{"data":{"status":"CLAIMED","benefit":{"amount":100}}}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	_, credits, _, err := d.Checkin(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if credits != 100 {
		t.Fatalf("reward = %d, want 100 from the enveloped benefit.amount", credits)
	}
}

// TestCampaignsClaimOtherStatusIsRefused: any status other than CLAIMED is a
// refusal. Recording it as a successful check-in would make the scheduler skip
// the rest of the day while the account never received the credits.
func TestCampaignsClaimOtherStatusIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]}`)
		case strings.Contains(r.URL.Path, "/campaigns/c1/claim"):
			fmt.Fprint(w, `{"status":"LIMIT_EXCEEDED"}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	_, _, _, err := d.Checkin(context.Background(), testQoderAccount(t))
	if err == nil {
		t.Fatal("a non-CLAIMED claim answer must be an error, not a recorded check-in")
	}
}

// TestCampaignsClaimedReadsNestedBenefit: an already-claimed day reports the
// campaign's own reward (the amount that WAS granted), read from the nested
// benefit object.
func TestCampaignsClaimedReadsNestedBenefit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			fmt.Fprint(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":100,"kind":"CREDITS"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	checkedIn, credits, _, err := d.Checkin(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !checkedIn {
		t.Fatal("CLAIMED must read as checked-in")
	}
	if credits != 100 {
		t.Fatalf("reward = %d, want 100 from the campaign's nested benefit.amount", credits)
	}
}
