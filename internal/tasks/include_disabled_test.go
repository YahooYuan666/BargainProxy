// ticket 32: keep-alive tasks may cover disabled accounts (switch off by
// default); travel and growth keep skipping them.
package tasks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/tasks"
)

// TestIncludeDisabledInTasksSwitch 开关关（默认）=禁用号跳过；开=签到等保号
// 任务覆盖禁用号（选号侧不受影响）。旅行/成长保持跳过。
func TestIncludeDisabledInTasksSwitch(t *testing.T) {
	sched, mock, p, _ := buildScheduler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p.SetDisabled("workbuddy:u-task", true)

	// Default off: the disabled account is skipped — nothing hits upstream.
	detail, err := sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, "")
	if err != nil {
		t.Fatalf("flag off run: %v (%s)", err, detail)
	}
	mock.mu.Lock()
	checkins := mock.checkins
	mock.mu.Unlock()
	if checkins != 0 {
		t.Fatalf("flag off: checkins = %d, want 0 (disabled skipped)", checkins)
	}
	if !strings.Contains(detail, "nothing to do") {
		t.Fatalf("flag off detail = %q, want nothing to do", detail)
	}

	// Switch on: the same disabled account now checks in.
	sched.SetIncludeDisabledInTasks(true)
	detail, err = sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, "")
	if err != nil {
		t.Fatalf("flag on run: %v (%s)", err, detail)
	}
	mock.mu.Lock()
	checkins = mock.checkins
	mock.mu.Unlock()
	if checkins != 1 {
		t.Fatalf("flag on: checkins = %d, want 1 (disabled covered)", checkins)
	}

	// Travel keeps skipping disabled accounts even with the switch on.
	detail, err = sched.RunTask(ctx, tasks.TaskTravelWorkBuddy, "")
	if err != nil {
		t.Fatalf("travel run: %v (%s)", err, detail)
	}
	mock.mu.Lock()
	travels := mock.departs
	mock.mu.Unlock()
	if travels != 0 {
		t.Fatalf("travel must stay disabled-skipping: departs = %d", travels)
	}
}
