package tasks

import (
	"testing"
	"time"

	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/store"
)

// The black_cat scoring window is 23:00 through 07:59 Asia/Shanghai.
func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour, minute int
		want         bool
	}{
		{22, 59, false},
		{23, 0, true},
		{23, 45, true},
		{0, 0, true},
		{7, 59, true},
		{8, 0, false},
		{12, 0, false},
	}
	for _, c := range cases {
		at := time.Date(2026, 10, 17, c.hour, c.minute, 0, 0, Shanghai)
		if got := inNightWindow(at); got != c.want {
			t.Fatalf("inNightWindow(%02d:%02d) = %v, want %v", c.hour, c.minute, got, c.want)
		}
	}
	// The window is evaluated on Shanghai wall clock regardless of the input
	// location: 15:00 UTC is 23:00 in Shanghai.
	utc := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	if !inNightWindow(utc) {
		t.Fatalf("inNightWindow(15:00 UTC) should be true (23:00 CST)")
	}
}

func TestBlackcatRemaining(t *testing.T) {
	cases := []struct {
		name  string
		tasks []workbuddy.GrowthTask
		want  int
	}{
		{"absent", []workbuddy.GrowthTask{{TaskCode: "chat_5", Progress: 1, Target: 5}}, 0},
		{"in progress", []workbuddy.GrowthTask{{TaskCode: "black_cat", Progress: 1, Target: 3}}, 2},
		{"complete", []workbuddy.GrowthTask{{TaskCode: "black_cat", Progress: 3, Target: 3}}, 0},
		{"over complete", []workbuddy.GrowthTask{{TaskCode: "black_cat", Progress: 4, Target: 3}}, 0},
		{"no target", []workbuddy.GrowthTask{{TaskCode: "black_cat", Progress: 0, Target: 0}}, 0},
	}
	for _, c := range cases {
		if got := blackcatRemaining(c.tasks); got != c.want {
			t.Fatalf("%s: blackcatRemaining = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestTaskTypesIncludeBlackcat(t *testing.T) {
	s := testScheduler(t, store.NewMemory())
	found := false
	for _, tt := range s.TaskTypes() {
		if tt == TaskBlackcatWorkBuddy {
			found = true
		}
	}
	if !found {
		t.Fatalf("TaskTypes missing %q: %v", TaskBlackcatWorkBuddy, s.TaskTypes())
	}
}
