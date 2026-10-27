package tasks

// BP-149: what a task can never act on, filtered out before dispatch.
//
// The "global realm has no …" answers used to come from inside each task
// function, and the caller recorded them as successful ledger rows. One global
// WorkBuddy account therefore added five "成功" rows a day (checkin, growth,
// activity, travel, night-owl) whose detail said nothing happened.
//
// This is a dispatch-time filter, so no row is written for an account the task
// would only have skipped. It is deliberately a narrow allow-list of known
// realm rules: an unknown task type keeps every account (never drop by
// accident).

import "bargainproxy/internal/model"

// noGlobalRealmTasks are the WorkBuddy tasks whose upstream build genuinely has
// no counterpart for an international (realm=global) account:
//
//   - checkin:      the global build has no daily check-in programme
//   - growth:       no growth centre ("成长中心")
//   - travel:       the cat-travel patrol lives in the growth centre
//   - blackcat:     the night-owl task is a growth task
//
// Activity reporting is deliberately NOT here: BP-117 measured that /v2/report
// answers code=0 on workbuddy.ai and lights the streak, so global accounts do
// report.
var noGlobalRealmTasks = map[string]bool{
	TaskCheckinWorkBuddy:  true,
	TaskGrowthWorkBuddy:   true,
	TaskTravelWorkBuddy:   true,
	TaskBlackcatWorkBuddy: true,
}

// filterActionable drops the accounts this task can never act on.
func (s *Scheduler) filterActionable(taskType string, accounts []*model.Account) []*model.Account {
	if !noGlobalRealmTasks[taskType] {
		return accounts
	}
	out := make([]*model.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Channel == model.ChannelWorkBuddy && a.Realm == "global" {
			continue
		}
		out = append(out, a)
	}
	return out
}
