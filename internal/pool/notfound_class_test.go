package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestNotFoundDoesNotCoolTheAccount pins the 2026-10-19 fix: a 404 is a
// property of the binding (path/model missing), identical for every account of
// the channel. Before this, the workbuddy driver filed 404 as model_rate and
// the pool cooled that account's model for up to ModelRateHoldCap (2h) — one
// routing break silently removed a healthy account from a model, and a
// channel-wide batch turned the pool into "model unavailable" ("switch
// models"), which is the wrong advice when the route is what is broken.
func TestNotFoundDoesNotCoolTheAccount(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), NotFoundPause: 60 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-1", model.ChannelWorkBuddy)
	p.Upsert(a)

	ue := uperr.New(uperr.ClassNotFound, 404, "workbuddy: 404 page not found")
	p.ApplyResult(a, "glm-5.3", ue)

	// No account-wide cooldown and, critically, no model cooldown.
	if !a.AccountCooldownUntil.IsZero() {
		t.Fatalf("404 must not set an account cooldown, got %v", a.AccountCooldownUntil)
	}
	if mc, ok := a.ModelCooldowns["glm-5.3"]; ok {
		t.Fatalf("404 must not create a model cooldown, got until=%v reason=%q", mc.Until, mc.Reason)
	}
	if a.ConsecutiveFails != 0 || a.BreakerFails != 0 {
		t.Fatalf("404 must not feed failure counters: consecutive=%d breaker=%d",
			a.ConsecutiveFails, a.BreakerFails)
	}
}

// TestNotFoundPausesTheChannelPair: the route stays dead for a moment, so the
// pool pauses channel+model briefly (shared state, not an account penalty) and
// the next request fails fast with the pause deadline instead of walking every
// account into the same 404.
func TestNotFoundPausesTheChannelPair(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cur := now
	p := New(Params{Now: func() time.Time { return cur }, NotFoundPause: 60 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-1", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassNotFound, 404, "404"))

	ok, earliest := p.NextAvailable(model.ChannelWorkBuddy, "glm-5.3")
	if ok {
		t.Fatal("channel+model must be paused right after a 404")
	}
	if earliest.Before(now.Add(30 * time.Second)) {
		t.Fatalf("pause deadline = %v, want roughly now+60s", earliest)
	}

	// After the pause expires the account serves again — nothing was cooled.
	cur = now.Add(61 * time.Second)
	ok, _ = p.NextAvailable(model.ChannelWorkBuddy, "glm-5.3")
	if !ok {
		t.Fatal("account must be usable again once the 404 pause expires (no cooldown was applied)")
	}
}

// TestNotFoundDoesNotRotate pins Rotatable: the next account of the same
// channel answers 404 too, so the binding loop must not walk the pool. It may
// still cascade to a different channel (handled by the caller).
func TestNotFoundDoesNotRotate(t *testing.T) {
	if uperr.Rotatable(uperr.ClassNotFound) {
		t.Fatal("404 must not rotate accounts within a channel")
	}
}

// TestNotFoundStillCascadesToAnotherChannel: the binding loop's cascade is
// gated on the class being non-rotatable only for SAME-channel rotation; a
// different channel is attempted while nothing reached the client. This is the
// documented contract in gateway: non-rotatable classes fall through only when
// the request may still be served elsewhere. Guard the taxonomy side here so a
// future change to Rotatable is a deliberate decision.
func TestNotFoundIsRequestLevelNotAccountLevel(t *testing.T) {
	if uperr.CooldownScope(uperr.ClassNotFound, true) != "account" {
		t.Fatal("not_found must not be reported as a model cooldown scope")
	}
	// Acquire must still succeed after ApplyResult (no state change).
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), NotFoundPause: time.Nanosecond},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-1", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassNotFound, 404, "404"))
	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "other-model", "")
	if err != nil {
		t.Fatalf("a different model must still be acquirable: %v", err)
	}
	lease.Release()
}
