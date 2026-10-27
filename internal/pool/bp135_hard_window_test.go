package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-135 (user ruling): antigravity's 5h/weekly quota buckets are HARD
// upstream-stated windows — honored as-is, never truncated by the 2h
// ModelRateHoldCap. Guessed/invented durations keep the cap.
func TestHardWindowHonoredAndGuessCapped(t *testing.T) {
	cur := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }, MaxQueueWait: time.Second},
		map[string]int{model.ChannelAntigravity: 2}, nil)
	acc := &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity}
	p.Upsert(acc)

	// Hard window: the body named the boundary — hold to the exact moment.
	hard := uperr.New(uperr.ClassSoftRate, 429, "antigravity: quota")
	hard.Model = "gemini-3-pro"
	hard.ResetAt = cur.Add(5 * time.Hour)
	hard.ResetHard = true
	p.ApplyResult(acc, "gemini-3-pro", hard)
	mc, ok := acc.ModelCooldowns["gemini-3-pro"]
	if !ok {
		t.Fatal("hard hold not recorded")
	}
	if !mc.Until.Equal(hard.ResetAt) {
		t.Fatalf("hard window Until = %s, want the exact upstream boundary %s", mc.Until, hard.ResetAt)
	}

	// The same shape WITHOUT the hard flag (our guess) keeps the 2h cap.
	guess := uperr.New(uperr.ClassSoftRate, 429, "antigravity: quota")
	guess.Model = "gemini-3-flash"
	guess.ResetAt = cur.Add(5 * time.Hour)
	p.ApplyResult(acc, "gemini-3-flash", guess)
	mc2 := acc.ModelCooldowns["gemini-3-flash"]
	if got := mc2.Until.Sub(cur); got != 2*time.Hour {
		t.Fatalf("guessed window Until = %s after cur, want the 2h cap", got)
	}
}
