package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

var sinkNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// TestRateLimitedAccountSinksBehindOthers pins BP-91: an account the upstream
// rate limited while it still had quota stays selectable but goes behind the
// ones that were not limited since. Without this the load returns to the same
// account the moment its rest ends, which is the pattern risk control reads as
// a bot.
func TestRateLimitedAccountSinksBehindOthers(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)},
		map[string]int{model.ChannelWorkBuddy: 4}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)

	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate limited"))
	if a.RateLimitedAt.IsZero() {
		t.Fatal("rate-limited account was not marked as sunk")
	}
	// Lift the rest so only the sunk ordering decides the pick.
	p.Lock()
	a.AccountCooldownUntil = time.Time{}
	p.Unlock()

	for i := 0; i < 8; i++ {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		got := lease.Account.ID
		p.Release(lease)
		if got == a.ID {
			t.Fatalf("pick %d chose the rate-limited account while %s was available", i, b.ID)
		}
	}

	// A successful answer clears the flag, so it rejoins the rotation.
	p.ApplyResult(a, "glm-5.3", nil)
	if !a.RateLimitedAt.IsZero() {
		t.Fatal("a successful answer must clear the sunk flag")
	}
}

// TestSunkAccountsAreStillTriedWhenEveryoneSank pins that sinking is an
// ordering, not a refusal: with every account sunk, the pool still serves.
func TestSunkAccountsAreStillTriedWhenEveryoneSank(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)},
		map[string]int{model.ChannelWorkBuddy: 4}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate limited"))
	p.ApplyResult(b, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate limited"))
	p.Lock()
	a.AccountCooldownUntil = time.Time{}
	b.AccountCooldownUntil = time.Time{}
	p.Unlock()

	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
	if err != nil {
		t.Fatalf("a fully sunk pool must still serve: %v", err)
	}
	p.Release(lease)
}

// TestSunkFlagExpires pins the TTL: an account that never gets picked again
// does not stay demoted forever.
func TestSunkFlagExpires(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate limited"))

	if ready := p.notSunkLocked([]*model.Account{a}, sinkNow); len(ready) != 0 {
		t.Fatal("a freshly sunk account must be filtered out")
	}
	if ready := p.notSunkLocked([]*model.Account{a}, sinkNow.Add(sinkTTL+time.Second)); len(ready) != 1 {
		t.Fatal("the sunk flag must expire after sinkTTL")
	}
}

// TestSinkOrderFollowsLimitOrder pins the ordering among sunk accounts: the
// one that sank first is preferred, so repeated rounds rotate rather than
// pinning one account.
func TestSinkOrderFollowsLimitOrder(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "first"))
	p.ApplyResult(b, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "second"))
	if !(a.SinkSeq < b.SinkSeq) {
		t.Fatalf("sink order should follow the limit order: a=%d b=%d", a.SinkSeq, b.SinkSeq)
	}
}
