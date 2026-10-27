package trae

// BP-147: a claim's own code 0 is not proof the day is in.
//
// Trae answers code 0 to a claim that registered nothing, and the claim body
// usually carries no credits at all. Success is what the STATUS page says
// after the claim; the credits we report are the ones that page shows, not the
// pre-claim read (magpie trae 0.2.22 / #333).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

type checkinMock struct {
	claimed    bool
	claims     int
	statusHit  int
	claimFails string // raw claim body when it must not land
	afterCreds int64
	afterLand  bool // whether the claim registers (false = silent drop)
}

func (m *checkinMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/trae/api/v2/ug/checkin_credits/status":
			m.statusHit++
			if m.claimed {
				fmt.Fprintf(w, `{"checked_in":%v,"credits":%d,"enable":true}`, m.afterLand, m.afterCreds)
				return
			}
			fmt.Fprint(w, `{"checked_in":false,"credits":100,"enable":true}`)
		case r.URL.Path == "/trae/api/v2/ug/checkin_credits/claim":
			m.claims++
			if m.claimFails != "" {
				fmt.Fprint(w, m.claimFails)
				return
			}
			m.claimed = true
			fmt.Fprint(w, `{"code":0}`) // no credits in the body — upstream's usual shape
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

// TestClaimCreditsComeFromTheStatusReRead: the reported balance must be the
// post-claim one, not the stale pre-claim value.
func TestClaimCreditsComeFromTheStatusReRead(t *testing.T) {
	m := &checkinMock{afterLand: true, afterCreds: 200}
	srv := m.server(t)
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	ok, credits, enable, err := d.Checkin(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !ok || !enable {
		t.Fatalf("checkedIn=%v enable=%v", ok, enable)
	}
	if credits != 200 {
		t.Fatalf("credits = %d, want 200 (post-claim status, not the stale 100)", credits)
	}
	if m.statusHit != 2 {
		t.Fatalf("status reads = %d, want 2 (before and after the claim)", m.statusHit)
	}
}

// TestSilentDropIsNotReportedAsClaimed is the bug this ticket exists for: the
// claim answers code 0 but the day never registers. Reporting "claimed +100"
// put money in the ledger that never arrived.
func TestSilentDropIsNotReportedAsClaimed(t *testing.T) {
	m := &checkinMock{afterLand: false, afterCreds: 100}
	srv := m.server(t)
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	ok, credits, _, err := d.Checkin(context.Background(), testAccount())
	if ok {
		t.Fatalf("a claim that registered nothing was reported as checked in")
	}
	if credits != 0 {
		t.Fatalf("credits = %d, want 0 for a drop", credits)
	}
	if err == nil {
		t.Fatalf("a silent drop must surface as an error so the ledger does not claim success")
	}
	if m.claims != 1 {
		t.Fatalf("claims = %d, want 1 (a drop is not re-sent today)", m.claims)
	}
}

// TestUnreadableStatusAfterClaimKeepsTheSuccess: a failed re-read is a network
// problem, not evidence the claim failed — do not turn possible credit into a
// red row.
func TestUnreadableStatusAfterClaimKeepsTheSuccess(t *testing.T) {
	var claimed bool
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trae/api/v2/ug/checkin_credits/status":
			hits++
			if hits == 1 {
				fmt.Fprint(w, `{"checked_in":false,"credits":100,"enable":true}`)
				return
			}
			w.WriteHeader(http.StatusInternalServerError) // the re-read dies
			fmt.Fprint(w, `oops`)
		case "/trae/api/v2/ug/checkin_credits/claim":
			claimed = true
			fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	ok, credits, _, err := d.Checkin(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("an unreadable status re-read must not fail the checkin: %v", err)
	}
	if !ok || !claimed {
		t.Fatalf("checkedIn=%v claimed=%v", ok, claimed)
	}
	if credits != 100 {
		t.Fatalf("credits = %d, want the last known balance 100", credits)
	}
}

// TestAlreadyCheckedInSkipsTheClaim: the early-exit path must not claim twice.
func TestAlreadyCheckedInSkipsTheClaim(t *testing.T) {
	m := &checkinMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/trae/api/v2/ug/checkin_credits/status" {
			fmt.Fprint(w, `{"checked_in":true,"credits":300,"enable":true}`)
			return
		}
		m.claims++
	}))
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	ok, credits, _, err := d.Checkin(context.Background(), testAccount())
	if err != nil || !ok || credits != 300 {
		t.Fatalf("ok=%v credits=%d err=%v", ok, credits, err)
	}
	if m.claims != 0 {
		t.Fatalf("claims = %d, want 0 when the day is already in", m.claims)
	}
}
