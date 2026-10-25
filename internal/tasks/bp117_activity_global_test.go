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

// BP-117: GLOBAL accounts must report activity.
//
// This task used to skip them on the belief that the international build has
// no activity system. The reference project tested that belief and corrected
// it in place, beside the very same skip: /v2/report answers code=0 on
// workbuddy.ai and lights the streak up. Skipping it means an international
// account can never light its streak — while doing it right costs one small
// request per day.
//
// The pool holds exactly one account here (the fixture's CN account is
// removed) so the assertion is about the GATE, not about mock throughput.
func TestActivityReportsForGlobalAccounts(t *testing.T) {
	sched, mock, p, _ := buildScheduler(t)
	p.Remove("workbuddy:u-task")
	creds, _ := json.Marshal(map[string]any{
		"access_token": testToken("wb"), "refresh_token": testToken("refresh"),
		"uid": "u-glb", "realm": "global", "nickname": "glb-acct",
	})
	p.Upsert(&model.Account{
		ID: "workbuddy:u-glb", Channel: model.ChannelWorkBuddy,
		CredsJSON: creds, UID: "u-glb", Realm: "global", Nickname: "glb-acct",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskActivityWorkBuddy, "")
	if err != nil {
		t.Fatalf("activity task: %v (%s)", err, detail)
	}
	if strings.Contains(detail, "skipped (global") {
		t.Fatalf("global account was skipped: %q — the international build accepts /v2/report", detail)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.reports < 1 {
		t.Fatalf("reports = %d, want >=1 for a global account", mock.reports)
	}
}

// ENTERPRISE accounts still skip: /v2/report 200s there, but everything it
// lights is 403 for them ("growth system is only available for personal
// users"), so the request is pure waste.
func TestActivitySkipsEnterpriseAccounts(t *testing.T) {
	sched, mock, p, _ := buildScheduler(t)
	p.Remove("workbuddy:u-task")
	creds, _ := json.Marshal(map[string]any{
		"access_token": testToken("wb"), "refresh_token": testToken("refresh"),
		"uid": "u-ent", "realm": "cn", "nickname": "ent-acct", "enterprise_id": "ent-42",
	})
	p.Upsert(&model.Account{
		ID: "workbuddy:u-ent", Channel: model.ChannelWorkBuddy,
		CredsJSON: creds, UID: "u-ent", Realm: "cn", Nickname: "ent-acct",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskActivityWorkBuddy, "")
	if err != nil {
		t.Fatalf("activity task: %v (%s)", err, detail)
	}
	if !strings.Contains(detail, "enterprise") {
		t.Fatalf("detail = %q, want the enterprise skip note", detail)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.reports != 0 {
		t.Fatalf("reports = %d, want 0 for an enterprise account", mock.reports)
	}
}
