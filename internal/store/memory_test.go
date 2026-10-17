package store

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// TestMemoryPurgeTaskRuns: the 14-day ledger cleanup must drop rows that
// started before the cutoff and keep everything at or after it, returning
// the number of removed rows (ticket 11 / review P2: the acceptance criteria
// required a memory-backend unit test next to the dual-backend purge).
func TestMemoryPurgeTaskRuns(t *testing.T) {
	m := NewMemory()
	cutoff := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	old := &model.TaskRun{AccountID: "wb:u1", TaskType: "checkin_workbuddy", RunDate: "2026-09-30",
		Status: "ok", StartedAt: cutoff.Add(-14 * 24 * time.Hour), FinishedAt: cutoff.Add(-14*24*time.Hour + time.Minute)}
	fresh := &model.TaskRun{AccountID: "wb:u1", TaskType: "checkin_workbuddy", RunDate: "2026-10-14",
		Status: "ok", StartedAt: cutoff, FinishedAt: cutoff.Add(time.Minute)}

	if err := m.UpsertTaskRun(old); err != nil {
		t.Fatalf("upsert old: %v", err)
	}
	if err := m.UpsertTaskRun(fresh); err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}

	n, err := m.PurgeTaskRuns(cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged rows = %d, want 1 (only the pre-cutoff row)", n)
	}

	// The fresh row survives; the old one is gone from the ledger.
	runs, err := m.RecentTaskRuns(10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(runs) != 1 || runs[0].RunDate != "2026-10-14" {
		t.Fatalf("post-purge ledger = %+v, want only the fresh row", runs)
	}
	if _, err := m.TaskRunFor("wb:u1", "checkin_workbuddy", "2026-09-30"); err == nil {
		t.Fatal("pre-cutoff row must be gone from TaskRunFor too")
	}

	// A cutoff past everything purges nothing and reports zero.
	n, err = m.PurgeTaskRuns(cutoff.Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("empty purge = (%d, %v), want (0, nil)", n, err)
	}

	// A cutoff past the last row drops it and reports one.
	n, err = m.PurgeTaskRuns(cutoff.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("final purge = (%d, %v), want (1, nil)", n, err)
	}
	runs, _ = m.RecentTaskRuns(10)
	if len(runs) != 0 {
		t.Fatalf("ledger not empty after full purge: %+v", runs)
	}
}
