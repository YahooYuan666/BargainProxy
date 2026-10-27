// BP-149 end-to-end: a global WorkBuddy account must leave no ledger row for
// the growth family, and a cn account on the same channel must still run.
package tasks_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/tasks"
)

func TestGlobalAccountLeavesNoGrowthLedgerRow(t *testing.T) {
	sched, _, p, st := buildScheduler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Turn the fixture's cn account into a global one — same channel, only the
	// realm differs. It is the exact shape the user has.
	gcreds, _ := json.Marshal(map[string]any{
		"access_token": "tok-wb", "refresh_token": "tok-refresh",
		"uid": "u-global", "realm": "global", "nickname": "g-acct",
	})
	p.Upsert(&model.Account{
		ID: "workbuddy:u-global", Channel: model.ChannelWorkBuddy,
		CredsJSON: gcreds, UID: "u-global", Realm: "global", Nickname: "g-acct",
	})
	p.Remove("workbuddy:u-task")

	detail, err := sched.RunTask(ctx, tasks.TaskGrowthWorkBuddy, "")
	if err != nil {
		t.Fatalf("RunTask: %v (%s)", err, detail)
	}
	if !strings.Contains(detail, "nothing to do") {
		t.Fatalf("detail = %q, want a nothing-to-do summary (no account was eligible)", detail)
	}

	runs, err := st.RecentTaskRuns(100)
	if err != nil {
		t.Fatalf("list task runs: %v", err)
	}
	for _, r := range runs {
		if r.TaskType == tasks.TaskGrowthWorkBuddy {
			t.Fatalf("a global-only channel wrote a growth ledger row: %+v", r)
		}
	}
}

// The other half: when the channel also holds a cn account, the task runs for
// that one and still leaves the global one alone.
func TestGlobalIsSkippedButCnStillRuns(t *testing.T) {
	sched, mock, p, st := buildScheduler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	gcreds, _ := json.Marshal(map[string]any{
		"access_token": "tok-wb", "refresh_token": "tok-refresh",
		"uid": "u-global", "realm": "global", "nickname": "g-acct",
	})
	p.Upsert(&model.Account{
		ID: "workbuddy:u-global", Channel: model.ChannelWorkBuddy,
		CredsJSON: gcreds, UID: "u-global", Realm: "global", Nickname: "g-acct",
	})

	detail, err := sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, "")
	if err != nil {
		t.Fatalf("RunTask: %v (%s)", err, detail)
	}

	runs, err := st.RecentTaskRuns(100)
	if err != nil {
		t.Fatalf("list task runs: %v", err)
	}
	var ran int
	for _, r := range runs {
		if r.TaskType != tasks.TaskCheckinWorkBuddy {
			continue
		}
		ran++
		if r.AccountID == "workbuddy:u-global" {
			t.Fatalf("the global account got a checkin row: %+v", r)
		}
	}
	if ran == 0 {
		t.Fatalf("the cn account did not run (no checkin row at all); mock checkins=%d", mock.checkins)
	}
	if mock.checkins != 1 {
		t.Fatalf("checkins = %d, want 1 (only the cn account)", mock.checkins)
	}
}
