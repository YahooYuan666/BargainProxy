package tasks

// BP-149: do not dispatch a daily task for an account that can never run it.
//
// The "global realm has no growth center" family was answered from inside each
// task function, and the caller recorded it as an "ok" ledger row. One global
// account on WorkBuddy therefore produced five "成功" rows a day whose detail
// said nothing happened (checkin, growth, activity, travel, night-owl), which
// turns the task history into noise and buries real failures.
//
// Filtering happens before dispatch, so those rows are never written. An
// account-targeted run (a panel click) still reaches the task function and
// gets the honest "skipped" answer — the user asked about that account.

import (
	"testing"

	"bargainproxy/internal/model"
)

func globalWB(id string) *model.Account {
	return &model.Account{ID: id, Channel: model.ChannelWorkBuddy, Realm: "global", Nickname: id}
}

func cnWB(id string) *model.Account {
	return &model.Account{ID: id, Channel: model.ChannelWorkBuddy, Realm: "cn", Nickname: id}
}

func TestFilterActionableDropsGlobalForGrowthFamily(t *testing.T) {
	accts := []*model.Account{globalWB("g1"), cnWB("c1"), globalWB("g2")}
	for _, task := range []string{
		TaskGrowthWorkBuddy, TaskTravelWorkBuddy, TaskBlackcatWorkBuddy, TaskCheckinWorkBuddy,
	} {
		s := &Scheduler{}
		got := s.filterActionable(task, accts)
		if len(got) != 1 || got[0].ID != "c1" {
			t.Errorf("%s: kept %d accounts (%v), want only c1", task, len(got), ids(got))
		}
	}
}

// TestFilterActionableKeepsGlobalForActivity: BP-117 established that global
// accounts DO report activity, so that task must still reach them.
func TestFilterActionableKeepsGlobalForActivity(t *testing.T) {
	accts := []*model.Account{globalWB("g1"), cnWB("c1")}
	s := &Scheduler{}
	got := s.filterActionable(TaskActivityWorkBuddy, accts)
	if len(got) != 2 {
		t.Fatalf("activity kept %d accounts (%v), want both", len(got), ids(got))
	}
}

// TestFilterActionableLeavesOtherChannelsAlone: only the workbuddy growth
// family has this realm rule.
func TestFilterActionableLeavesOtherChannelsAlone(t *testing.T) {
	trae := &model.Account{ID: "t1", Channel: model.ChannelTrae, Realm: "global"}
	s := &Scheduler{}
	got := s.filterActionable(TaskCheckinTrae, []*model.Account{trae})
	if len(got) != 1 {
		t.Fatalf("trae checkin lost its account: %v", ids(got))
	}
}

// TestFilterActionablePassthroughForUnknownTask: a task with no rule keeps all
// accounts (never drop silently by accident).
func TestFilterActionablePassthroughForUnknownTask(t *testing.T) {
	accts := []*model.Account{globalWB("g1"), cnWB("c1")}
	s := &Scheduler{}
	if got := s.filterActionable(TaskProbeAll, accts); len(got) != 2 {
		t.Fatalf("probe lost accounts: %v", ids(got))
	}
}
