package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// TestSingleActivePerChannelHolderTakesMoreWork pins the clarified principle
// (2026-10-21): one account per pool may interact at a time, and THAT account
// may run 2-3 tasks concurrently — a second request rides the SAME account
// (within max_in_flight) instead of being refused.
func TestSingleActivePerChannelHolderTakesMoreWork(t *testing.T) {
	p := New(Params{SingleActivePerChannel: true, MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 3}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	a.Nickname = "account-a"
	p.Upsert(a)

	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-2")
	if err != nil {
		t.Fatalf("second acquire on the SAME account must succeed: %v", err)
	}
	if l2.Account.ID != l1.Account.ID {
		t.Fatalf("second request must land on the holder (%s), got %s", l1.Account.ID, l2.Account.ID)
	}
	l2.Release()
}

// TestSingleActivePerChannelRoutesAway: once the holder is FULL, a further
// request is refused (ErrChannelBusy, fast) so the gateway walks to the next
// pool instead of starting a second account here.
func TestSingleActivePerChannelRoutesAway(t *testing.T) {
	p := New(Params{SingleActivePerChannel: true, MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	a.Nickname = "account-a"
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)

	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-2")
	if err != nil {
		t.Fatalf("second acquire (holder has free slot) must succeed: %v", err)
	}
	if l2.Account.ID != l1.Account.ID {
		t.Fatalf("second request must stay on the holder, got %s (holder %s)", l2.Account.ID, l1.Account.ID)
	}
	l3, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-3")
	if err == nil {
		l3.Release()
		t.Fatal("third acquire (holder full) must be refused")
	}
	var busy *ErrChannelBusy
	if !errors.As(err, &busy) {
		t.Fatalf("error = %v, want ErrChannelBusy (route to next pool, not wait)", err)
	}
	start := time.Now()
	_, _ = p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-4")
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("busy refusal took %s: it must not queue", elapsed)
	}
	if ok, _ := p.ChannelBusy(model.ChannelWorkBuddy); !ok {
		t.Fatal("ChannelBusy must report the pool as interacting")
	}
}

// TestSingleActiveReleasedReopensPool: once the interacting request finishes,
// the pool takes work again.
func TestSingleActiveReleasedReopensPool(t *testing.T) {
	p := New(Params{SingleActivePerChannel: true, MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	p.Upsert(mkAccount("wb-a", model.ChannelWorkBuddy))

	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	lease.Release()

	lease2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	lease2.Release()
}

// TestMultiAccountConcurrencyWhenDisabled: the old behaviour stays available
// behind the switch (explicit opt-out).
func TestMultiAccountConcurrencyWhenDisabled(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	p.Upsert(mkAccount("wb-a", model.ChannelWorkBuddy))
	p.Upsert(mkAccount("wb-b", model.ChannelWorkBuddy))

	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("second acquire with the switch off must succeed: %v", err)
	}
	l2.Release()
}
