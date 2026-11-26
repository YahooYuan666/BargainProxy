package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-345, the field case (2026-11-25 22:31–22:40). ONE account per pool is
// the user's standing rule and is not touched here. The defect was what
// happened to the second conversation:
//
//	conversation 1 rides account A on glm-5.3-flash (long turn, still running)
//	conversation 2 is bound to account A too and asks for glm-5.4
//	account A's glm-5.4 hits the daily wall (810000)
//
// Conversation 2's binding is dropped (A cannot serve glm-5.4 any more), so it
// falls to "select another account" — where single-active refuses because A
// is still interacting for conversation 1. The refusal was terminal, so the
// turn ended with an error although account B was idle, healthy, and had
// glm-5.4. Eighteen requests failed that way in nine minutes.
//
// The single-active rule forbids STARTING a second account while A is busy.
// It does not require failing: the request can WAIT for A to finish, and then
// B is the only account interacting. One account at a time, still.

// walledPool builds the field state: A holds the pool with conversation 1 in
// flight, conversation 2 is bound to A, and A's glm-5.4 is walled until the
// next day. B is idle and healthy.
func walledPool(t *testing.T, maxWait time.Duration) (*Pool, []*Lease, *model.Account, *model.Account) {
	t.Helper()
	p := New(Params{SingleActivePerChannel: true, MaxQueueWait: maxWait},
		map[string]int{model.ChannelAutoClaw: 2}, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	b := mkAccount("ac-b", model.ChannelAutoClaw)
	p.Upsert(a)

	// Conversation 2 binds to A: with A the only account in the pool there is
	// nowhere else for it to go (a new conversation spreads to the least-held
	// account, BP-234).
	l2, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.4", "conv-2")
	if err != nil {
		t.Fatalf("conv-2: %v", err)
	}
	if l2.Account.ID != a.ID {
		l2.Release()
		t.Fatalf("conv-2 must bind to %s, got %s", a.ID, l2.Account.ID)
	}
	l2.Release()

	// Conversation 1 starts on A, another model, and stays in flight. A second
	// long turn takes A's last slot (max_in_flight is 2), so the holder is
	// FULL: the state in which single-active used to answer "busy" to
	// everything, the walled model included.
	l1, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.3-flash", "conv-1")
	if err != nil {
		t.Fatalf("conv-1: %v", err)
	}
	if l1.Account.ID != a.ID {
		l1.Release()
		t.Fatalf("conv-1 must ride the holder %s, got %s", a.ID, l1.Account.ID)
	}
	l1b, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.3-flash", "conv-1b")
	if err != nil {
		l1.Release()
		t.Fatalf("conv-1b: %v", err)
	}
	if l1b.Account.ID != a.ID {
		l1.Release()
		l1b.Release()
		t.Fatalf("conv-1b must ride the holder %s, got %s", a.ID, l1b.Account.ID)
	}

	p.Upsert(b)

	// A's glm-5.4 hits the daily wall.
	ue := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: GLM-5.4 今日用量额度已用完（810000）")
	ue.Code = "810000"
	ue.Model = "glm-5.4"
	ue.ResetAt = NextDay4AM(time.Now())
	ue.ResetHard = true
	p.ApplyResult(a, "glm-5.4", ue)
	// held releases every lease that keeps account A interacting.
	return p, []*Lease{l1, l1b}, a, b
}

func releaseAll(held []*Lease) {
	for _, l := range held {
		l.Release()
	}
}

func TestWalledModelWaitsForThePoolThenMovesToAnotherAccount(t *testing.T) {
	p, held, _, b := walledPool(t, 2*time.Second)

	type got struct {
		lease *Lease
		err   error
	}
	done := make(chan got, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		lease, err := p.Acquire(ctx, model.ChannelAutoClaw, "glm-5.4", "conv-2")
		done <- got{lease, err}
	}()

	// While conversation 1 still runs, conversation 2 must neither fail nor
	// start account B: it waits.
	select {
	case g := <-done:
		if g.lease != nil {
			g.lease.Release()
		}
		t.Fatalf("conv-2 returned while account A was still interacting (err=%v): a second account must not start, and the conversation must not end", g.err)
	case <-time.After(300 * time.Millisecond):
	}
	// The pool is still held for every model A CAN serve: the single-active
	// rule is intact for ordinary traffic.
	if ok, _ := p.ChannelBusy(model.ChannelAutoClaw, "glm-5.3-flash"); !ok {
		t.Fatal("ChannelBusy(glm-5.3-flash) = false; account A's in-flight requests must still hold the pool for the model it serves")
	}
	// For the WALLED model the pool is not "busy": A could never serve it, so
	// "go elsewhere" is no answer — this is what lets the gateway send the
	// request into Acquire to wait instead of skipping the pool.
	if ok, _ := p.ChannelBusy(model.ChannelAutoClaw, "glm-5.4"); ok {
		t.Fatal("ChannelBusy(glm-5.4) = true; the holder cannot serve that model, so the pool must not report busy for it")
	}
	// And with no model the channel-wide answer keeps the old meaning.
	if ok, _ := p.ChannelBusy(model.ChannelAutoClaw, ""); !ok {
		t.Fatal("the channel-wide question must still report the pool as interacting")
	}

	// Conversation 1 finishes: the pool opens and conv-2 moves to B.
	releaseAll(held)
	select {
	case g := <-done:
		if g.err != nil {
			t.Fatalf("conv-2 after the pool opened: %v (the walled account must not end the conversation)", g.err)
		}
		if g.lease.Account.ID != b.ID {
			g.lease.Release()
			t.Fatalf("conv-2 landed on %s, want the un-walled account %s", g.lease.Account.ID, b.ID)
		}
		g.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("conv-2 was still waiting after the pool opened")
	}
}

// The wait is bounded: if the holder never finishes, Acquire gives up with a
// capacity error (so the gateway cascades / reports honestly) — it never
// hangs.
func TestWalledModelWaitIsBounded(t *testing.T) {
	p, held, _, _ := walledPool(t, 250*time.Millisecond)
	defer releaseAll(held)

	start := time.Now()
	lease, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.4", "conv-2")
	if err == nil {
		lease.Release()
		t.Fatal("expected a refusal once the bounded wait ran out")
	}
	var nc *ErrNoCapacity
	if !errors.As(err, &nc) {
		t.Fatalf("error = %v (%T), want ErrNoCapacity (the bounded wait ran out)", err, err)
	}
	if el := time.Since(start); el < 200*time.Millisecond || el > 1500*time.Millisecond {
		t.Fatalf("refused after %s, want about MaxQueueWait", el)
	}
}

// A client that gives up (its context is cancelled) leaves the wait at once.
func TestWalledModelWaitHonoursClientCancel(t *testing.T) {
	p, held, _, _ := walledPool(t, 5*time.Second)
	defer releaseAll(held)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		lease, err := p.Acquire(ctx, model.ChannelAutoClaw, "glm-5.4", "conv-2")
		if lease != nil {
			lease.Release()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("acquire succeeded after the client left")
		}
	case <-time.After(time.Second):
		t.Fatal("the wait did not observe the client's cancellation")
	}
}

// The single-active rule itself is untouched: with NO walled model involved, a
// request that would start a second account is still refused at once and
// never queues (TestSingleActivePerChannelRoutesAway pins the same thing from
// the gateway's side; this is the guard that BP-345 did not loosen it).
func TestUnwalledRequestStillRefusedWithoutQueueing(t *testing.T) {
	p := New(Params{SingleActivePerChannel: true, MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelAutoClaw: 1}, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	b := mkAccount("ac-b", model.ChannelAutoClaw)
	p.Upsert(a)
	p.Upsert(b)

	l1, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.3-flash", "conv-1")
	if err != nil {
		t.Fatalf("conv-1: %v", err)
	}
	defer l1.Release()

	start := time.Now()
	_, err = p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.4", "conv-9")
	if err == nil {
		t.Fatal("a second account must not start")
	}
	var busy *ErrChannelBusy
	if !errors.As(err, &busy) {
		t.Fatalf("error = %v (%T), want ErrChannelBusy", err, err)
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Fatalf("refusal took %s: an ordinary busy refusal must not queue", el)
	}
}
