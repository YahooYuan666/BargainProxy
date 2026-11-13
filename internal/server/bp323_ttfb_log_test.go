// BP-323: 反应时间 must report how long the UPSTREAM took to start answering,
// not how long the reasoning hold lasted.
//
// The hold (gateway.go) keeps reasoning back until the answer starts, so every
// ingress's own TTFB — measured on the first event it FORWARDS — now means
// "wait for the answer". The panel divides output tokens by (duration - ttfb)
// for its token rate and shows ttfb as 反应时间, so the substitution corrupts
// two columns at once.
//
// Field 2026-11-13 21:46 (before the fix): a 24.5s request logged
// ttfb=22118ms with 559 output tokens — the 22s of reasoning sat in neither
// the TTFB nor the generation window.
package server

import (
	"testing"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
)

// TestLogRowPrefersUpstreamTTFB is the headline: when the gateway measured the
// upstream's real first byte, that value wins over the ingress's
// first-forwarded-event time (which the caller already put in the row).
func TestLogRowPrefersUpstreamTTFB(t *testing.T) {
	row := &model.RequestLog{TTFBMs: 22118} // the ingress's own, hold-delayed value
	logResult(row, &gateway.Result{
		Channel:           model.ChannelWorkBuddy,
		Account:           &model.Account{ID: "wb-1"},
		UpstreamTTFB:      2400 * time.Millisecond,
		UpstreamTTFBKnown: true,
	}, time.Now())
	if row.TTFBMs != 2400 {
		t.Fatalf("TTFBMs = %d, want the upstream's real 2400ms (not the hold-delayed 22118ms)", row.TTFBMs)
	}
}

// A leg that produced no content event leaves the flag clear and the ingress
// value stands — "no measurement" must not become a fabricated zero.
func TestLogRowKeepsIngressTTFBWhenUnmeasured(t *testing.T) {
	row := &model.RequestLog{TTFBMs: 1234}
	logResult(row, &gateway.Result{
		Channel: model.ChannelWorkBuddy,
		Account: &model.Account{ID: "wb-1"},
	}, time.Now())
	if row.TTFBMs != 1234 {
		t.Fatalf("TTFBMs = %d, want the ingress value 1234 kept (no upstream measurement)", row.TTFBMs)
	}
}

// A genuinely zero measurement (a first byte that lands within the clock's
// resolution) must still be recorded as measured, not discarded — the flag,
// not the value, decides.
func TestLogRowRecordsZeroUpstreamTTFB(t *testing.T) {
	row := &model.RequestLog{TTFBMs: 999}
	logResult(row, &gateway.Result{
		Channel:           model.ChannelWorkBuddy,
		Account:           &model.Account{ID: "wb-1"},
		UpstreamTTFB:      0,
		UpstreamTTFBKnown: true,
	}, time.Now())
	if row.TTFBMs != 0 {
		t.Fatalf("TTFBMs = %d, want 0 (measured, and the upstream was immediate)", row.TTFBMs)
	}
}
