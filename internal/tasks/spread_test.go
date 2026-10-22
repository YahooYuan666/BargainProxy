package tasks

import (
	"testing"
	"time"
)

// TestSpreadOffsetsAreStableForTheDay pins BP-96: an account's slot comes from
// (task, account, date), so a restart does not reshuffle the schedule.
func TestSpreadOffsetsAreStableForTheDay(t *testing.T) {
	s := &Scheduler{accountSpread: 5 * time.Minute, accountSpreadGap: 30 * time.Second}
	ids := []string{"wb-1", "wb-2", "wb-3"}
	a := s.spreadOffsets(TaskCheckinWorkBuddy, ids)
	b := s.spreadOffsets(TaskCheckinWorkBuddy, ids)
	if len(a) != len(ids) || len(b) != len(ids) {
		t.Fatalf("got %d/%d offsets for %d ids", len(a), len(b), len(ids))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("offset %d not stable: %v vs %v", i, a[i], b[i])
		}
		if a[i] < 0 || a[i] >= 5*time.Minute {
			t.Fatalf("offset %d = %v outside the window", i, a[i])
		}
	}
}

// TestSpreadOffsetsKeepMinGap pins the collision rule: no two accounts of one
// run land closer than the minimum gap, and none runs past the window.
func TestSpreadOffsetsKeepMinGap(t *testing.T) {
	s := &Scheduler{accountSpread: 5 * time.Minute, accountSpreadGap: 30 * time.Second}
	ids := []string{"wb-1", "wb-2", "wb-3", "wb-4", "wb-5"}
	offsets := s.spreadOffsets(TaskCheckinWorkBuddy, ids)
	sorted := append([]time.Duration(nil), offsets...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	for i := 1; i < len(sorted); i++ {
		if gap := sorted[i] - sorted[i-1]; gap < 30*time.Second {
			t.Fatalf("two accounts are %v apart, want at least 30s (offsets %v)", gap, offsets)
		}
	}
	if last := sorted[len(sorted)-1]; last >= 5*time.Minute {
		t.Fatalf("last slot %v runs past the window (offsets %v)", last, offsets)
	}
}

// TestSpreadDisabledForManualRuns pins that a panel click is immediate, and
// that one account or a zero window never waits.
func TestSpreadDisabledForManualRuns(t *testing.T) {
	s := &Scheduler{accountSpread: 5 * time.Minute, accountSpreadGap: 30 * time.Second}
	if got := s.spreadOffsets(TaskCheckinWorkBuddy, nil); got != nil {
		t.Fatalf("manual run got offsets %v, want none", got)
	}
	if got := s.spreadOffsets(TaskCheckinWorkBuddy, []string{"wb-1"}); got != nil {
		t.Fatalf("single account got offsets %v, want none", got)
	}
	s.accountSpread = 0
	if got := s.spreadOffsets(TaskCheckinWorkBuddy, []string{"wb-1", "wb-2"}); got != nil {
		t.Fatalf("zero window got offsets %v, want none", got)
	}
}

// TestSpreadFallsBackWhenWindowIsTooSmall pins the degenerate case: more
// accounts than the window can hold still get distinct, evenly spaced slots.
func TestSpreadFallsBackWhenWindowIsTooSmall(t *testing.T) {
	s := &Scheduler{accountSpread: 60 * time.Second, accountSpreadGap: 30 * time.Second}
	got := s.spreadOffsets(TaskCheckinWorkBuddy, []string{"wb-1", "wb-2", "wb-3", "wb-4"})
	want := []time.Duration{0, 30 * time.Second, 60 * time.Second, 90 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offsets = %v, want %v", got, want)
		}
	}
}

// TestSpreadDiffersAcrossAccounts pins that the slots are not all the same
// value: a hash that collided everywhere would be an even gap in disguise.
func TestSpreadDiffersAcrossAccounts(t *testing.T) {
	s := &Scheduler{accountSpread: 10 * time.Minute, accountSpreadGap: 5 * time.Second}
	offsets := s.spreadOffsets(TaskCheckinWorkBuddy, []string{"wb-1", "wb-2", "wb-3"})
	distinct := map[time.Duration]bool{}
	for _, d := range offsets {
		distinct[d] = true
	}
	if len(distinct) != len(offsets) {
		t.Fatalf("offsets collided: %v", offsets)
	}
}
