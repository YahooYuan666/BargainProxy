package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-213: opencodezen's free lane has ONE daily allowance per egress IP,
// shared by every caller — not this account's own quota. A 429 carrying a
// SharedQuota mark must fall back to the ordinary short backoff, never rest
// the account until the stated midnight: that turned a single 429 into a
// channel-wide outage that outlived the failure (field 2026-10-30: every
// later request answered "cooling down" without reaching upstream).
func TestBP213SharedQuotaDoesNotRestUntilTheStatedReset(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	p := New(Params{
		Now:              func() time.Time { return now },
		SoftCooldownBase: time.Minute,
	}, nil, nil)
	a := mkAccount("zen-213", model.ChannelOpenCodeZen)
	// A stale breaker from earlier account-owned failures must not lengthen a
	// shared-allowance wait (BP-215).
	a.BreakerFails = 3
	p.Upsert(a)

	ue := uperr.New(uperr.ClassSoftRate, 429, "opencodezen: free usage limit reached")
	ue.ResetAt = now.Add(9 * time.Hour) // the stated 00:00 UTC wall
	ue.SharedQuota = true
	p.ApplyResult(a, "big-pickle", ue)

	// The rest must be the short backoff, NOT the nine-hour wall.
	if a.AccountCooldownUntil.After(now.Add(2 * time.Minute)) {
		t.Fatalf("a shared-quota 429 rested the account until %v (the stated reset %v) — it must use the short backoff",
			a.AccountCooldownUntil, ue.ResetAt)
	}
	if a.AccountCooldownReason != "shared quota" {
		t.Fatalf("reason = %q, want the shared-quota wording so the panel tells the truth", a.AccountCooldownReason)
	}
	// The honest wait still rides the error for clients and the panel.
	if ue.HoldUntil.IsZero() {
		t.Fatal("HoldUntil must be annotated so renderers can say when to retry")
	}
	// And the wait must be the plain base backoff, not the account's failure
	// ladder stretched by a stale breaker (BP-215): the shared wall is not
	// something this account's history can influence.
	if wait := ue.HoldUntil.Sub(now); wait > 2*time.Minute {
		t.Fatalf("shared-quota wait = %v, want the plain base backoff (a stale breaker must not stretch it)", wait)
	}
}

// A NON-shared soft rate keeps its upstream-stated reset: the old behaviour
// must not regress for allowances that really do belong to the account.
func TestBP213AccountOwnedSoftRateKeepsItsReset(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	p := New(Params{
		Now:              func() time.Time { return now },
		SoftCooldownBase: time.Minute,
	}, nil, nil)
	a := mkAccount("wb-213", model.ChannelWorkBuddy)
	p.Upsert(a)

	ue := uperr.New(uperr.ClassSoftRate, 429, "workbuddy: rate limited")
	ue.ResetAt = now.Add(30 * time.Minute)
	p.ApplyResult(a, "glm-5.3", ue)

	if a.AccountCooldownUntil.Before(now.Add(29 * time.Minute)) {
		t.Fatalf("an account-owned soft rate must keep its stated reset, got %v", a.AccountCooldownUntil)
	}
}
