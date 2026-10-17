// BP-41: budget → effort band mapping used by gear-only channels.
package ir

import "testing"

func TestLevelFromBudget(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, ""},
		{-5, ""},
		{1024, "low"},
		{4095, "low"},
		{4096, "medium"},
		{16383, "medium"},
		{16384, "high"},
		{31999, "high"},
	}
	for _, c := range cases {
		if got := LevelFromBudget(c.budget); got != c.want {
			t.Fatalf("LevelFromBudget(%d) = %q, want %q", c.budget, got, c.want)
		}
	}
}
