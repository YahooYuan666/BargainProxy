package qoder

// BP-151: the daily benefit refreshes at 10:00 UTC+8, and the check-in slots
// run at 01:05 / 11:05 / 21:05. The FIRST slot therefore always lands hours
// before the refresh. Reporting that as a settled "not available" ok locked
// the day's later slots behind the idempotency ledger, so the daily 100
// credits could only ever be claimed by hand.
//
// The fix: a benefit campaign that EXISTS but is not claimable yet is a
// RETRYABLE answer — the task ledger records a failure, and the day's later
// slots retry it (after 10:00 they succeed).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
)

// campaignsSrv serves the campaigns page with a configurable claimStatus and
// counts how often the claim endpoint was hit.
type campaignsSrv struct {
	status      string // the daily-check-in status endpoint's answer
	claimStatus string // the campaign's claimStatus
	claims      int
}

func (m *campaignsSrv) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer","expires_in":7200}`)
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprintf(w, `{"status":%q}`, m.status)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			fmt.Fprintf(w, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","startAt":0,"endAt":0,"claimStatus":%q,"benefit":{"amount":100}}]}`, m.claimStatus)
		case strings.HasSuffix(r.URL.Path, "/claim"):
			m.claims++
			fmt.Fprint(w, `{"status":"CLAIMED","benefit":{"amount":100}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func runCheckin(t *testing.T, srv *httptest.Server) (bool, int64, bool, error) {
	t.Helper()
	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL}, nil)
	return d.Checkin(context.Background(), testQoderAccount(t))
}

// TestBenefitNotClaimableYetIsRetryable is the core: the programme exists,
// the share is not on the counter — the answer must be an ERROR so the task
// ledger records a failure and the later slots retry.
func TestBenefitNotClaimableYetIsRetryable(t *testing.T) {
	m := &campaignsSrv{status: "DISABLED", claimStatus: "PENDING"}
	srv := m.server(t)
	defer srv.Close()

	ok, credits, _, err := runCheckin(t, srv)
	if err == nil {
		t.Fatalf("a not-yet-claimable benefit was reported as settled (ok=%v credits=%d) — that is what locked the day's slots", ok, credits)
	}
	if ok {
		t.Fatalf("checkedIn must be false while the share is missing")
	}
	if !strings.Contains(err.Error(), "not claimable yet") {
		t.Fatalf("error should say the benefit is not claimable yet: %v", err)
	}
	if m.claims != 0 {
		t.Fatalf("claim was attempted %d times on a PENDING campaign", m.claims)
	}
}

// TestBenefitWithNoCampaignStaysInactive: when the upstream serves NO
// CLAIM_BENEFIT campaign at all, that is a real "no programme" — it must stay
// a settled non-error, not a daily failure row.
func TestBenefitWithNoCampaignStaysInactive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer","expires_in":7200}`)
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			fmt.Fprint(w, `{"campaigns":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	ok, _, enable, err := runCheckin(t, srv)
	if err != nil {
		t.Fatalf("a channel with no benefit campaign must not error: %v", err)
	}
	_ = enable
	if ok {
		t.Fatalf("checkedIn must be false when nothing was claimed")
	}
	if enable {
		t.Fatalf("enable must be false so the task reports 'not available' (settled)")
	}
}

// TestClaimedBenefitStillSettles: once the day's benefit says CLAIMED, the
// answer stays settled (later slots must not re-claim).
func TestClaimedBenefitStillSettles(t *testing.T) {
	m := &campaignsSrv{status: "DISABLED", claimStatus: "CLAIMED"}
	srv := m.server(t)
	defer srv.Close()

	ok, credits, enable, err := runCheckin(t, srv)
	if err != nil {
		t.Fatalf("a CLAIMED day must not error: %v", err)
	}
	if !ok || !enable {
		t.Fatalf("ok=%v enable=%v, want true/true", ok, enable)
	}
	if credits != 100 {
		t.Fatalf("credits = %d, want 100 (the benefit amount)", credits)
	}
	if m.claims != 0 {
		t.Fatalf("claim attempted on an already-CLAIMED day")
	}
}

// TestClaimableBenefitClaimsAndReportsReward: the happy path claims once and
// reports the reward as "+N credits" phrasing (checkedIn=false), which is what
// taskRunCredits books into the ledger.
func TestClaimableBenefitClaimsAndReportsReward(t *testing.T) {
	m := &campaignsSrv{status: "DISABLED", claimStatus: "CLAIMABLE"}
	srv := m.server(t)
	defer srv.Close()

	ok, credits, enable, err := runCheckin(t, srv)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Fatalf("a fresh claim must report checkedIn=false so the ledger says '+100 credits', not 'already checked in'")
	}
	if credits != 100 {
		t.Fatalf("credits = %d, want 100", credits)
	}
	if !enable {
		t.Fatalf("enable = false, want true")
	}
	if m.claims != 1 {
		t.Fatalf("claims = %d, want 1", m.claims)
	}
}
