package agybridge

// Turn idle watchdog (BP-342), following upstream agy-acp
// (src/runtime/worker.ts armTurnIdleWatchdog / checkTurnIdle):
//
//	TURN_IDLE_BUDGET_MS       = 10 * 60 * 1000   // quiet generation
//	TURN_TOOL_IDLE_BUDGET_MS  = 60 * 60 * 1000   // a tool call may take far longer
//	TURN_IDLE_TICK_MS         = 30 * 1000
//
// Every frame agy emits re-arms it; only SILENCE retires the worker. Upstream's
// reasoning, kept verbatim because the failure it prevents is exactly what this
// project hit: "a 30-minute wall clock said nothing about activity: it could
// not tell a quiet generation from a dead one".
//
// Retirement (kill the child) rather than settling the turn is deliberate: a
// local timeout is not proof the native prompt stopped, and sending a second
// prompt to a live native session answers with whatever the first generation is
// still doing.

import (
	"sync"
	"time"
)

const (
	agyTurnIdleBudget     = 10 * time.Minute
	agyTurnToolIdleBudget = 60 * time.Minute
	agyTurnIdleTick       = 30 * time.Second
)

type idleWatchdog struct {
	mu       sync.Mutex
	last     time.Time
	deadline time.Time
	toolMode bool
	budgetOv time.Duration // test seam: overrides the budget
	tickOv   time.Duration // test seam: overrides the tick
	firedAt  chan struct{}
	stopOnce sync.Once
	stopCh   chan struct{}
}

// newIdleWatchdog starts the ticker for one turn.
func newIdleWatchdog(hasTools bool) *idleWatchdog {
	w := &idleWatchdog{
		last:     time.Now(),
		toolMode: hasTools,
		firedAt:  make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	w.deadline = w.last.Add(w.budget())
	go w.run()
	return w
}

func (w *idleWatchdog) budget() time.Duration {
	if w.budgetOv > 0 {
		return w.budgetOv
	}
	if w.toolMode {
		return agyTurnToolIdleBudget
	}
	return agyTurnIdleBudget
}

// rearm records progress: the next silence window starts now.
func (w *idleWatchdog) rearm() {
	w.mu.Lock()
	w.last = time.Now()
	w.mu.Unlock()
}

// fired closes when the budget elapsed with no progress.
func (w *idleWatchdog) fired() <-chan struct{} { return w.firedAt }

// stop ends the ticker (call when the turn finishes).
func (w *idleWatchdog) stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
}

func (w *idleWatchdog) run() {
	tick := agyTurnIdleTick
	if w.tickOv > 0 {
		tick = w.tickOv
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-t.C:
			w.mu.Lock()
			quiet := time.Since(w.last)
			budget := w.budget()
			w.mu.Unlock()
			if quiet >= budget {
				close(w.firedAt)
				return
			}
		}
	}
}
