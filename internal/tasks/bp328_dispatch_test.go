// BP-328: the scheduler's minute tick ran every task SYNCHRONOUSLY, so one
// slow task starved the whole schedule.
//
// Field evidence (2026-11-19 audit): probe_all takes 30–60 minutes per round
// (qoder/lobster/antigravity each burn their RPC timeout, and accounts are
// spread over a 5-minute window). While it ran, nothing else could fire:
// activity was configured for 03:25 and actually recorded 04:25; the 01:05
// check-in landed at 02:05. dueSlot's catch-up semantics mean no task is LOST,
// but a daily task that drifts an hour lands in a different traffic window —
// on 2026-11-17 it drifted straight into the .ai outage that cost four
// accounts their streak (BP-330).
//
// These tests drive the loop's dispatch decision directly: one task that never
// returns must not delay the next one, and a task still running must not be
// started twice.
package tasks

import (
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bp328Blocking is a task runner stub whose calls block until released, so a
// synchronous dispatcher would hang the test instead of running the next job.
type bp328Blocking struct {
	mu       sync.Mutex
	starts   map[string]int
	release  chan struct{}
	finished chan string
}

func newBP328Blocking() *bp328Blocking {
	return &bp328Blocking{
		starts:   map[string]int{},
		release:  make(chan struct{}),
		finished: make(chan string, 8),
	}
}

func (b *bp328Blocking) run(name string) {
	b.mu.Lock()
	b.starts[name]++
	b.mu.Unlock()
	<-b.release
	b.finished <- name
}

func (b *bp328Blocking) count(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts[name]
}

func newBP328Scheduler() *Scheduler {
	return &Scheduler{
		log:         log.New(io.Discard, "", 0),
		running:     map[string]bool{},
		taskEnabled: map[string]bool{},
	}
}

// TestBP328SlowTaskDoesNotStarveTheTick: while one task is still running, a
// second, independent task must still be dispatched.
func TestBP328SlowTaskDoesNotStarveTheTick(t *testing.T) {
	s := newBP328Scheduler()
	b := newBP328Blocking()
	t.Cleanup(func() { close(b.release) })

	// A probe round that never finishes within the test. Dispatched through a
	// goroutine here so that a REGRESSION to synchronous dispatch fails the
	// assertion below instead of hanging the whole test binary.
	go s.dispatch(TaskProbeAll, func() { b.run(TaskProbeAll) })
	// Wait for it to actually start before dispatching the next one.
	deadline := time.Now().Add(2 * time.Second)
	for b.count(TaskProbeAll) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first task never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The tick must still be able to dispatch the daily task: with a
	// synchronous dispatcher this call would block until the probe returned.
	dispatched := make(chan struct{})
	go func() {
		s.dispatch(TaskActivityWorkBuddy, func() { b.run(TaskActivityWorkBuddy) })
		close(dispatched)
	}()
	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch itself blocked while another task was running — the tick would be stuck for the whole probe round (BP-328)")
	}
	deadline = time.Now().Add(2 * time.Second)
	for b.count(TaskActivityWorkBuddy) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second task was not dispatched while the first was still running — one slow probe round starves every daily slot (BP-328)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := b.count(TaskActivityWorkBuddy); got != 1 {
		t.Fatalf("activity dispatches = %d, want 1", got)
	}
}

// TestBP328SameTaskIsNotDispatchedTwice: a second trigger for a task that is
// still running must be dropped (and logged), not queued — otherwise a long
// probe stacks round on round.
func TestBP328SameTaskIsNotDispatchedTwice(t *testing.T) {
	s := newBP328Scheduler()
	b := newBP328Blocking()
	t.Cleanup(func() { close(b.release) })

	s.dispatch(TaskProbeAll, func() { b.run(TaskProbeAll) })
	deadline := time.Now().Add(2 * time.Second)
	for b.count(TaskProbeAll) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first probe never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second trigger while the first is still in flight.
	s.dispatch(TaskProbeAll, func() { b.run(TaskProbeAll) })
	time.Sleep(100 * time.Millisecond)
	if got := b.count(TaskProbeAll); got != 1 {
		t.Fatalf("probe dispatches = %d, want 1 — a still-running task must not be re-dispatched (BP-328)", got)
	}

	// Once it finishes, the gate must open again.
	close(b.release)
	b.release = make(chan struct{})
	select {
	case <-b.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the first probe never finished")
	}
	s.dispatch(TaskProbeAll, func() { b.run(TaskProbeAll) })
	deadline = time.Now().Add(2 * time.Second)
	for b.count(TaskProbeAll) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the task never became dispatchable again after finishing")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBP328DisabledTaskIsNotDispatched: the panel switch keeps working, and a
// switched-off task is not counted as running.
func TestBP328DisabledTaskIsNotDispatched(t *testing.T) {
	s := newBP328Scheduler()
	s.taskEnabled = map[string]bool{TaskActivityWorkBuddy: false}

	var ran atomic.Bool
	s.dispatchIfEnabled(TaskActivityWorkBuddy, func() { ran.Store(true) })
	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a disabled task was dispatched")
	}
	if s.isRunning(TaskActivityWorkBuddy) {
		t.Fatal("a disabled task must not be marked running")
	}
}
