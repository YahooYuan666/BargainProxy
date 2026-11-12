package pool

// BP-314: a rate-limit verdict with NO upstream boundary must schedule a
// short probe, not the maximum hold.
//
// Field 2026-11-12 16:29:56: Google answered a bare 429 RESOURCE_EXHAUSTED
// with no retryDelay for gemini-3.8-flash-high. The driver's parseQuotaReset
// returned the zero time, and setModelCooldownHard did this:
//
//	until := resetAt                       // zero
//	if cap := ModelRateHoldCap; ... {       // 2h
//	    if c := now.Add(cap); until.IsZero() || c.Before(until) {
//	        until = c                       // ← zero read as "no constraint"
//	    }
//	}
//	if until.IsZero() || !until.After(now) { until = now.Add(60s) }  // dead code
//
// So the model was held for the full 2h cap while the 60s fallback below it
// could never run. The panel then said "quota-exhausted until 18:29:56" and the
// client was told "all accounts are cooling down or exhausted" — for a wobble
// that had nothing to do with the account's ledger (five_hour was at 100%,
// weekly at 95.6%, and the same accounts answered other models in ~1.1s).

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// The reported case: zero reset must produce the SHORT bounded hold.
func TestZeroResetGetsShortHoldNotTheCap(t *testing.T) {
	cur := time.Date(2026, 11, 12, 16, 29, 56, 0, time.Local)
	p := New(Params{Now: func() time.Time { return cur }, MaxQueueWait: time.Second},
		map[string]int{model.ChannelAntigravity: 2}, nil)
	acc := &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity}
	p.Upsert(acc)

	e := uperr.New(uperr.ClassSoftRate, 429, "antigravity: RESOURCE_EXHAUSTED, no retryDelay")
	e.Model = "gemini-3.8-flash-high"
	// ResetAt deliberately left zero: the body named no boundary.
	p.ApplyResult(acc, "gemini-3.8-flash-high", e)

	mc, ok := acc.ModelCooldowns["gemini-3.8-flash-high"]
	if !ok {
		t.Fatal("no model cooldown recorded for the 429")
	}
	if got := mc.Until.Sub(cur); got != 60*time.Second {
		t.Fatalf("hold = %s, want the 60s probe window; a zero reset is our GUESS "+
			"and must not become the %s cap (that is what made 16:29 look like a 2h quota wall)",
			got, 2*time.Hour)
	}
	// The client-facing barrier must agree with the account state.
	if e.HoldUntil.IsZero() || !e.HoldUntil.Equal(mc.Until) {
		t.Fatalf("HoldUntil = %s, want it to match Until = %s", e.HoldUntil, mc.Until)
	}
}

// The cap must still bound a LONG GUESSED reset (an upstream number that is
// not flagged as the bucket boundary stays capped) — the fix must not remove
// the cap, only stop it from firing on an unknown boundary.
func TestLongGuessedResetStillCapped(t *testing.T) {
	cur := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	p := New(Params{Now: func() time.Time { return cur }, MaxQueueWait: time.Second},
		map[string]int{model.ChannelAntigravity: 2}, nil)
	acc := &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity}
	p.Upsert(acc)

	e := uperr.New(uperr.ClassSoftRate, 429, "antigravity: quota")
	e.Model = "gemini-3.8-flash-high"
	e.ResetAt = cur.Add(5 * time.Hour) // upstream said 5h, but not as a hard flag
	p.ApplyResult(acc, "gemini-3.8-flash-high", e)

	mc := acc.ModelCooldowns["gemini-3.8-flash-high"]
	if got := mc.Until.Sub(cur); got != 2*time.Hour {
		t.Fatalf("guessed long window = %s after now, want the 2h cap", got)
	}
}

// A HARD upstream window is still honored exactly (BP-135 must not regress).
func TestHardWindowStillExact(t *testing.T) {
	cur := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	p := New(Params{Now: func() time.Time { return cur }, MaxQueueWait: time.Second},
		map[string]int{model.ChannelAntigravity: 2}, nil)
	acc := &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity}
	p.Upsert(acc)

	reset := cur.Add(5 * time.Hour)
	e := uperr.New(uperr.ClassSoftRate, 429, "antigravity: quota")
	e.Model = "gemini-3.8-flash-high"
	e.ResetAt = reset
	e.ResetHard = true
	p.ApplyResult(acc, "gemini-3.8-flash-high", e)

	if got := acc.ModelCooldowns["gemini-3.8-flash-high"].Until; !got.Equal(reset) {
		t.Fatalf("hard window Until = %s, want the exact upstream boundary %s", got, reset)
	}
}

// An EXPIRED reset (upstream named a moment already past) behaves like an
// unknown boundary: short probe, not cap.
func TestExpiredResetGetsShortHold(t *testing.T) {
	cur := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	p := New(Params{Now: func() time.Time { return cur }, MaxQueueWait: time.Second},
		map[string]int{model.ChannelAntigravity: 2}, nil)
	acc := &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity}
	p.Upsert(acc)

	e := uperr.New(uperr.ClassSoftRate, 429, "antigravity: quota")
	e.Model = "gemini-3.8-flash-high"
	e.ResetAt = cur.Add(-5 * time.Minute) // already elapsed
	p.ApplyResult(acc, "gemini-3.8-flash-high", e)

	if got := acc.ModelCooldowns["gemini-3.8-flash-high"].Until.Sub(cur); got != 60*time.Second {
		t.Fatalf("hold = %s, want 60s for an already-expired reset", got)
	}
}
