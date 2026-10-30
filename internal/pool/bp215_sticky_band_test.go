package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-215: breaker_fails had no way back to zero once the account stopped being
// offered work. The counter is cleared only by a successful answer, while
// admission refuses new conversations from an account whose counter reached
// StickyFailThreshold — so the account could never produce the success that
// would clear it. Field 2026-10-30: opencodezen sat at breaker_fails=2, every
// request answered "all accounts are cooling down; earliest reset at <now>",
// nothing reached upstream, and restarts did not help because the counter is
// persisted and reloaded by Upsert.
func TestBP215StickyBandLiftsInsteadOfDeadlocking(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	clock := now
	p := New(Params{
		Now:                 func() time.Time { return clock },
		StickyGrace:         2 * time.Minute,
		StickyFailThreshold: 2,
		MaxQueueWait:        time.Second,
	}, nil, nil)
	a := mkAccount("zen-215", model.ChannelOpenCodeZen)
	a.BreakerFails = 2
	a.UpdatedAt = now
	p.Upsert(a)

	// Right after the failures the account is held back from new conversations.
	if got := p.AdmissionOf(a, "big-pickle"); got != AdmitSticky {
		t.Fatalf("admission = %v, want sticky-only inside the grace", got)
	}
	if _, err := p.Acquire(context.Background(), model.ChannelOpenCodeZen, "big-pickle", ""); err == nil {
		t.Fatal("a sticky-only account must not be handed a brand-new conversation")
	}

	// The band must LIFT by itself once the failures age out; otherwise the
	// account is out of routing for good.
	clock = now.Add(3 * time.Minute)
	lease, err := p.Acquire(context.Background(), model.ChannelOpenCodeZen, "big-pickle", "")
	if err != nil {
		t.Fatalf("the sticky band never lifted — the account is deadlocked out of routing: %v", err)
	}
	p.Release(lease)
}

// The band is a grace window measured from the NEWEST failure, not a permanent
// amnesty and not a one-shot flag: another failure inside the window restarts
// it. (Measured on admissionAt because the counter alone cannot say when.)
func TestBP215TheWindowFollowsTheNewestFailure(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	p := New(Params{
		Now:                 func() time.Time { return now },
		StickyGrace:         2 * time.Minute,
		StickyFailThreshold: 2,
	}, nil, nil)
	a := mkAccount("zen-215b", model.ChannelOpenCodeZen)
	a.BreakerFails = 2
	a.UpdatedAt = now
	p.Upsert(a)

	if _, lift := p.admissionAt(a, "big-pickle", now); !lift.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("lift = %v, want %v", lift, now.Add(2*time.Minute))
	}
	a.UpdatedAt = now.Add(90 * time.Second) // a newer failure lands
	if _, lift := p.admissionAt(a, "big-pickle", now); !lift.Equal(now.Add(90*time.Second + 2*time.Minute)) {
		t.Fatalf("lift = %v, want it to follow the newest failure", lift)
	}
}

// A success clears the counter and the band at once — the grace window is not
// the only way back, and a recovered account must not sit out the remainder.
func TestBP215SuccessClearsTheBandImmediately(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	p := New(Params{
		Now:                 func() time.Time { return now },
		StickyGrace:         2 * time.Minute,
		StickyFailThreshold: 2,
	}, nil, nil)
	a := mkAccount("zen-215e", model.ChannelOpenCodeZen)
	a.BreakerFails = 5
	a.UpdatedAt = now
	p.Upsert(a)

	p.ApplyResult(a, "big-pickle", nil)
	if got := p.AdmissionOf(a, "big-pickle"); got != AdmitNew {
		t.Fatalf("admission after a success = %v, want schedulable", got)
	}
}

// BP-215: a shared-quota 429 belongs to the egress, not the account. Charging
// the breaker for it is what put opencodezen's only account at 2 and started
// the deadlock above.
func TestBP215SharedQuotaDoesNotRaiseTheBreaker(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	p := New(Params{
		Now:              func() time.Time { return now },
		SoftCooldownBase: time.Minute,
	}, nil, nil)
	a := mkAccount("zen-215c", model.ChannelOpenCodeZen)
	p.Upsert(a)

	for i := 0; i < 5; i++ {
		ue := uperr.New(uperr.ClassSoftRate, 429, "opencodezen: free usage limit reached")
		ue.ResetAt = now.Add(9 * time.Hour)
		ue.SharedQuota = true
		p.ApplyResult(a, "big-pickle", ue)
	}
	if a.BreakerFails != 0 {
		t.Fatalf("breaker_fails = %d after five shared-quota 429s; the account is not the party at fault and the counter feeds admission",
			a.BreakerFails)
	}
}

// The refusal must name the moment it lifts. Reporting "now" told the client to
// retry immediately into the same refusal, so a two-minute band read as a dead
// channel.
func TestBP215NoCapacityReportsTheBandLiftNotNow(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	p := New(Params{
		Now:                 func() time.Time { return now },
		StickyGrace:         2 * time.Minute,
		StickyFailThreshold: 2,
	}, nil, nil)
	a := mkAccount("zen-215d", model.ChannelOpenCodeZen)
	a.BreakerFails = 2
	a.UpdatedAt = now
	p.Upsert(a)

	ok, earliest := p.NextAvailable(model.ChannelOpenCodeZen, "big-pickle")
	if ok {
		t.Fatal("NextAvailable said a sticky-only account is available for a new conversation")
	}
	if want := now.Add(2 * time.Minute); !earliest.Equal(want) {
		t.Fatalf("earliest = %v, want the band lift %v (reporting now makes clients hot-loop)", earliest, want)
	}
}
