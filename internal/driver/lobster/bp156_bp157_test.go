package lobster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-156: 40201 "免费额度已用完" rides an event:error frame on an HTTP 200
// stream. It must be a balance death (long cooldown, rotate) — the old path
// handed the client a bare 400 and kept the drained account selectable.
func TestBP156FreeQuotaExhaustedIsHardCredit(t *testing.T) {
	e := classifyCode(40201, "免费额度已用完，请升级套餐")
	if e.Class != uperr.ClassHardCredit {
		t.Fatalf("40201 became %s, want hard_credit", e.Class)
	}
	// The chat SSE error path funnels through frameErrorOf — verify the
	// frame-shaped body reaches the same class.
	fe := frameErrorOf(`{"error":{"code":40201,"message":"免费额度已用完，请升级套餐"}}`)
	if fe == nil || fe.Class != uperr.ClassHardCredit {
		t.Fatalf("SSE frame 40201 → %v, want hard_credit", fe)
	}
}

// BP-157: a claim POST that answers 200 while the state still says
// claimedToday=false did NOT land (idempotent replay, reconfigured activity).
// Reporting success would hide a missing reward.
func TestBP157ClaimWithoutLandingIsAnError(t *testing.T) {
	contextReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/client-activities/slot"):
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act","configRevision":1}}}`)
		case strings.HasSuffix(r.URL.Path, "/context"):
			contextReads++
			io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":false,"claimedDays":1,"totalDays":36500}}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/check_in"):
			// The lie: ok-shaped response, but its own context admits nothing
			// was granted.
			io.WriteString(w, `{"code":0,"data":{"replayed":true,"result":{"creditsGranted":0},"context":{"state":{"claimedToday":false,"claimedDays":1,"totalDays":36500}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	_, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err == nil {
		t.Fatal("a claim that did not land must not report success")
	}
	if !strings.Contains(err.Error(), "claimedToday is still false") {
		t.Fatalf("error lost the reason: %v", err)
	}
	if contextReads < 2 {
		t.Fatalf("the false answer must trigger one re-read (got %d context reads)", contextReads)
	}
}

// The honest case is untouched: a claim whose context says claimedToday=true
// succeeds, and the day counters come back.
func TestBP157LandedClaimStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/client-activities/slot"):
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act","configRevision":1}}}`)
		case strings.HasSuffix(r.URL.Path, "/context"):
			io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":false,"claimedDays":1,"totalDays":36500}}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/check_in"):
			io.WriteString(w, `{"code":0,"data":{"result":{"creditsGranted":100},"context":{"state":{"claimedToday":true,"claimedDays":2,"totalDays":36500}}}}`)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	res, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err != nil {
		t.Fatalf("landed claim errored: %v", err)
	}
	if res.CreditsGranted != 100 || res.ClaimedDays != 2 {
		t.Fatalf("result = %+v", res)
	}
}

// An unreadable post-claim context keeps the old behaviour: POST success
// means claimed (no invented failures from a flaky re-read).
func TestBP157UnreadableContextKeepsSuccess(t *testing.T) {
	broken := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/client-activities/slot"):
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act","configRevision":1}}}`)
		case strings.HasSuffix(r.URL.Path, "/context"):
			if broken {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":false,"claimedDays":1,"totalDays":36500}}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/check_in"):
			io.WriteString(w, `{"code":0,"data":{"result":{"creditsGranted":100},"context":{"state":{"claimedToday":false}}}}`)
			broken = true // the re-read after the claim fails
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	res, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err != nil {
		t.Fatalf("unreadable re-read must keep the success: %v", err)
	}
	if res.CreditsGranted != 100 {
		t.Fatalf("credits = %d", res.CreditsGranted)
	}
}
