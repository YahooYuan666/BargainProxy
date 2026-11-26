package agybridge

import (
	"testing"
	"time"
)

// The watchdog fires on SILENCE only, and any progress re-arms it.
func TestIdleWatchdogFiresOnSilence(t *testing.T) {
	w := newIdleWatchdog(false)
	defer w.stop()
	w.budgetOv = 150 * time.Millisecond
	w.tickOv = 20 * time.Millisecond
	w.mu.Lock()
	w.last = time.Now()
	w.mu.Unlock()
	select {
	case <-w.fired():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire after the budget elapsed")
	}
}

func TestIdleWatchdogRearms(t *testing.T) {
	w := newIdleWatchdog(false)
	defer w.stop()
	w.budgetOv = 400 * time.Millisecond
	w.tickOv = 20 * time.Millisecond
	for i := 0; i < 5; i++ {
		w.rearm()
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-w.fired():
		t.Fatal("watchdog fired despite continuous progress")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestIdleWatchdogToolBudget(t *testing.T) {
	w := newIdleWatchdog(true)
	defer w.stop()
	if got := w.budget(); got != agyTurnToolIdleBudget {
		t.Fatalf("tool budget = %s, want %s", got, agyTurnToolIdleBudget)
	}
	w2 := newIdleWatchdog(false)
	defer w2.stop()
	if got := w2.budget(); got != agyTurnIdleBudget {
		t.Fatalf("plain budget = %s, want %s", got, agyTurnIdleBudget)
	}
}
