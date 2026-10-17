package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestGreedyFloorHotUpdateChangesRanking: the greedy floor is hot-settable
// and demonstrably changes which account the greedy picker prefers.
// Scores: max(secs-to-reset, floor) divides the weekly fraction.
//
//	z: frac 0.30, resets in 100s  -> default floor 600 -> 0.0005
//	a: frac 1.00, resets in 2000s -> default floor 600 -> 0.0005 (tie -> ID)
//
// With the floor dropped to 50s, z scores 0.003 > a's 0.0005 and wins.
func TestGreedyFloorHotUpdateChangesRanking(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelAntigravity: 5}, nil)

	z := mkAccount("z", model.ChannelAntigravity)
	z.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.30, ResetAt: now.Add(100 * time.Second), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.50, ResetAt: now.Add(2 * time.Hour), UpdatedAt: now},
	}
	a := mkAccount("a", model.ChannelAntigravity)
	a.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 1.00, ResetAt: now.Add(2000 * time.Second), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.90, ResetAt: now.Add(2 * time.Hour), UpdatedAt: now},
	}
	p.Upsert(z)
	p.Upsert(a)

	pick := func() string {
		// Reset pick timestamps so the 100ms anti-collision gap cannot
		// influence the pick — this test is about the floor, not the gap.
		p.mu.Lock()
		z.LastUsedAt, a.LastUsedAt = time.Time{}, time.Time{}
		p.mu.Unlock()
		lease, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		id := lease.Account.ID
		lease.Release()
		return id
	}

	if got := pick(); got != "a" {
		t.Fatalf("default floor tie-break should pick a, got %q", got)
	}
	p.SetGreedyFloor(50 * time.Second)
	if got := pick(); got != "z" {
		t.Fatalf("floor=50s should prefer z (soon-resetting, still-burnable), got %q", got)
	}
}

// TestSoftCooldownBaseHotUpdate: new soft cooldowns use the hot-set base.
func TestSoftCooldownBaseHotUpdate(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	acc := mkAccount("w1", model.ChannelWorkBuddy)
	p.Upsert(acc)

	// Default base 600s, no ResetAt on the error -> cooldown until now+600s.
	p.ApplyResult(acc, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate limited"))
	want := now.Add(600 * time.Second)
	if !acc.AccountCooldownUntil.Equal(want) {
		t.Fatalf("default cooldown = %s, want %s", acc.AccountCooldownUntil, want)
	}

	// Hot-update to 120s, fresh account -> now+120s.
	p.SetSoftCooldownBase(120 * time.Second)
	acc2 := mkAccount("w2", model.ChannelWorkBuddy)
	p.Upsert(acc2)
	p.ApplyResult(acc2, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate limited"))
	want2 := now.Add(120 * time.Second)
	if !acc2.AccountCooldownUntil.Equal(want2) {
		t.Fatalf("hot cooldown = %s, want %s", acc2.AccountCooldownUntil, want2)
	}
}

// TestChannelLimitHotUpdateWakesWaiter: raising a channel limit lets a queued
// request proceed immediately (in-flight requests are unaffected).
func TestChannelLimitHotUpdateWakesWaiter(t *testing.T) {
	p := New(Params{MaxQueueWait: 5 * time.Second}, map[string]int{model.ChannelWorkBuddy: 1}, nil)
	p.Upsert(mkAccount("w1", model.ChannelWorkBuddy))

	lease1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lease1.Release()

	type res struct {
		lease *Lease
		err   error
	}
	done := make(chan res, 1)
	go func() {
		l, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
		done <- res{l, err}
	}()

	// The waiter is parked on the limit; raising it must wake it promptly.
	time.Sleep(50 * time.Millisecond)
	p.SetLimit(model.ChannelWorkBuddy, 2)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("waiter after limit raise: %v", r.err)
		}
		r.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken by SetLimit")
	}
}
