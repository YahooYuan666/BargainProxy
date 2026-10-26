package tasks

import (
	"strings"
	"testing"
)

// BP-121: every registered check-in task must actually be fired by the timer.
//
// Field 2026-10-26: task_runs held zero rows for checkin_lobster. The lobster
// driver had a complete Checkin (每日积分礼, +100 credits/day), the task type
// was in TaskTypes(), the per-account handler was wired into RunTask — and the
// daily fan-out called only four of the five channels, so the task could only
// ever be started by hand. The user's report was "Lobster 账号也有每日签到功能
// 应该是也没做", which is exactly what an unscheduled task looks like.
func TestEveryRegisteredCheckinIsScheduled(t *testing.T) {
	scheduled := map[string]bool{}
	for _, tt := range checkinFanOutTasks() {
		scheduled[tt] = true
	}
	var registered []string
	for _, tt := range (&Scheduler{}).TaskTypes() {
		if strings.HasPrefix(tt, "checkin_") {
			registered = append(registered, tt)
		}
	}
	if len(registered) == 0 {
		t.Fatal("no checkin_* task types registered at all")
	}
	for _, tt := range registered {
		if !scheduled[tt] {
			t.Errorf("%s is registered and has a handler, but the daily timer never fires it", tt)
		}
	}
	// And the reverse: the fan-out must not name a task type that does not
	// exist (that would be a silent no-op in RunTask).
	known := map[string]bool{}
	for _, tt := range (&Scheduler{}).TaskTypes() {
		known[tt] = true
	}
	for _, tt := range checkinFanOutTasks() {
		if !known[tt] {
			t.Errorf("fan-out names unknown task type %q", tt)
		}
	}
}

// TestLobsterCheckinIsInFanOut pins the specific regression: this exact string
// was missing.
func TestLobsterCheckinIsInFanOut(t *testing.T) {
	found := false
	for _, tt := range checkinFanOutTasks() {
		if tt == TaskCheckinLobster {
			found = true
		}
	}
	if !found {
		t.Fatal("checkin_lobster missing from the daily fan-out (the task can then never run)")
	}
}
