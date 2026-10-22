package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/store"
)

// Qoder-7a red-first test (BP-98): a check-in's credit reward must land in
// TaskRun.Credits, not only in the detail string — the task ledger is the
// only place a "签到收益趋势" could be computed from, and Credits was
// always zero.

func TestTaskRunCreditsParsedFromDetail(t *testing.T) {
	s := &Scheduler{
		st:      store.NewMemory(),
		pl:      pool.New(pool.Params{}, map[string]int{}, nil),
		running: map[string]bool{},
	}
	acc := &model.Account{ID: "qoder:cr", Channel: model.ChannelQoder, Nickname: "t"}
	s.pl.Upsert(acc)

	_, err := s.runPerAccount(context.Background(), TaskCheckinQoder, "qoder:cr", model.ChannelQoder,
		func(ctx context.Context, acc *model.Account) (string, error) {
			return "+100 credits claimed", nil
		})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	today := time.Now().In(Shanghai).Format("2006-01-02")
	run, err := s.st.TaskRunFor("qoder:cr", TaskCheckinQoder, today)
	if err != nil {
		t.Fatalf("task run: %v", err)
	}
	if run.Credits != 100 {
		t.Fatalf("TaskRun.Credits = %d, want 100 (the reward must reach the ledger)", run.Credits)
	}
	if !strings.Contains(run.Detail, "100") {
		t.Fatalf("detail lost the reward: %q", run.Detail)
	}
}

func TestTaskRunCreditsZeroWhenUnstated(t *testing.T) {
	s := &Scheduler{
		st:      store.NewMemory(),
		pl:      pool.New(pool.Params{}, map[string]int{}, nil),
		running: map[string]bool{},
	}
	acc := &model.Account{ID: "trae:cr", Channel: model.ChannelTrae, Nickname: "t"}
	s.pl.Upsert(acc)

	_, err := s.runPerAccount(context.Background(), TaskCheckinTrae, "trae:cr", model.ChannelTrae,
		func(ctx context.Context, acc *model.Account) (string, error) {
			return "already checked in today", nil
		})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	today := time.Now().In(Shanghai).Format("2006-01-02")
	run, err := s.st.TaskRunFor("trae:cr", TaskCheckinTrae, today)
	if err != nil {
		t.Fatalf("task run: %v", err)
	}
	if run.Credits != 0 {
		t.Fatalf("no reward stated: Credits = %d, want 0", run.Credits)
	}
}
