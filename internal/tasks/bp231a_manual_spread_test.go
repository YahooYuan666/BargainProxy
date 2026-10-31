package tasks

import (
	"context"
	"log"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/store"
)

func testLogger() *log.Logger { return log.New(nil, "", 0) }

func bp231aScheduler(t *testing.T) *Scheduler {
	t.Helper()
	pl := pool.New(pool.Params{}, map[string]int{model.ChannelZCode: 2}, nil)
	pl.Upsert(&model.Account{ID: "zcode:t1", Channel: model.ChannelZCode})
	pl.Upsert(&model.Account{ID: "zcode:t2", Channel: model.ChannelZCode})
	s := &Scheduler{pl: pl, st: store.NewMemory(), log: testLogger()}
	s.running = map[string]bool{}
	s.accountSpread = 3 * time.Minute
	s.accountSpreadGap = 30 * time.Second
	s.accountGap = 800 * time.Millisecond
	return s
}

// BP-231a: a MANUAL run (panel "run now", marked WithoutSpread) dispatches
// BOTH accounts back-to-back — the 3m19s spread wait against the caller's
// request context is what turned every manual checkin_zcode click into
// "context canceled" ledger rows that looked like a network fault. A
// scheduled run keeps the spread: the second account holds for its ≥30s gap.
func TestBP231aManualRunSkipsTheSpread(t *testing.T) {
	s := bp231aScheduler(t)
	runs := make(chan string, 4)
	fn := func(ctx context.Context, a *model.Account) (string, error) {
		runs <- a.ID
		return "ran", nil
	}

	// Manual: both accounts dispatched within seconds.
	_, _ = s.runPerAccount(WithoutSpread(context.Background()), "checkin_zcode", "", model.ChannelZCode, fn)
	deadline := time.After(5 * time.Second)
	got := 0
collect:
	for {
		select {
		case <-runs:
			got++
			if got == 2 {
				break collect
			}
		case <-deadline:
			t.Fatalf("manual run dispatched only %d/2 accounts within 5s", got)
		}
	}

	// Scheduled: unmarked ctx — the spread holds the second account for at
	// least its 30s gap, so within 3s at most ONE account has run.
	s2 := bp231aScheduler(t)
	runs2 := make(chan string, 4)
	fn2 := func(ctx context.Context, a *model.Account) (string, error) {
		runs2 <- a.ID
		return "ran", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = s2.runPerAccount(ctx, "checkin_zcode", "", model.ChannelZCode, fn2)
	deadline2 := time.After(3 * time.Second)
	got2 := 0
drain:
	for {
		select {
		case <-runs2:
			got2++
		case <-deadline2:
			break drain
		}
	}
	if got2 > 1 {
		t.Fatalf("scheduled run dispatched %d accounts within 3s — the spread must hold the second", got2)
	}
}

// A caller disconnect during the spread wait must NOT dispatch the task's
// first network call against a dead context — accounts are skipped and no
// misleading "canceled" ledger row is written.
func TestBP231aDeadContextSkipsDispatch(t *testing.T) {
	s := bp231aScheduler(t)
	ran := make(chan struct{}, 4)
	fn := func(ctx context.Context, a *model.Account) (string, error) {
		ran <- struct{}{}
		return "ran", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before the run starts: the worst case of a client hang-up
	_, _ = s.runPerAccount(ctx, "checkin_zcode", "", model.ChannelZCode, fn)
	select {
	case <-ran:
		t.Fatal("fn must not run against a canceled context")
	default:
	}
}
