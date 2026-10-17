package tasks

import (
	"reflect"
	"testing"

	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

// Defaults mirror the shipped schedule (agentpoints 定时任务参数对齐).
func TestTaskScheduleParamsDefaults(t *testing.T) {
	s := testScheduler(t, store.NewMemory())
	p := s.TaskScheduleParams()
	if !reflect.DeepEqual(p.CheckinHours, []int{9, 21}) {
		t.Fatalf("default checkin hours = %v, want [9 21]", p.CheckinHours)
	}
	if !reflect.DeepEqual(p.GrowthHours, []int{9}) {
		t.Fatalf("default growth hours = %v, want [9]", p.GrowthHours)
	}
	// agentpoints 口径：活跃上报默认 10 点，猫猫旅行默认 9,21 点。
	if !reflect.DeepEqual(p.ActivityHours, []int{10}) {
		t.Fatalf("default activity hours = %v, want [10]", p.ActivityHours)
	}
	if !reflect.DeepEqual(p.TravelHours, []int{9, 21}) {
		t.Fatalf("default travel hours = %v, want [9 21]", p.TravelHours)
	}
	if !reflect.DeepEqual(p.BlackcatHours, []int{23}) {
		t.Fatalf("default blackcat hours = %v, want [23]", p.BlackcatHours)
	}
	if p.ProbeMinutes != 5 || p.GoogleProbeMinutes != 15 || p.RefreshMinutes != 60 {
		t.Fatalf("default intervals = %+v", p)
	}
}

// Set persists and hot-applies; LoadSettings restores on a fresh scheduler
// (settings table is the restart source of truth).
func TestTaskScheduleParamsRoundTrip(t *testing.T) {
	st := store.NewMemory()
	s := testScheduler(t, st)
	in := model.TaskParams{
		CheckinHours: []int{22, 10}, GrowthHours: []int{1},
		ActivityHours: []int{12}, TravelHours: []int{8, 20}, BlackcatHours: []int{23, 1},
		ProbeMinutes: 3, GoogleProbeMinutes: 20, RefreshMinutes: 90,
	}
	if err := s.SetTaskScheduleParams(in); err != nil {
		t.Fatalf("set: %v", err)
	}
	got := s.TaskScheduleParams()
	if !reflect.DeepEqual(got.CheckinHours, []int{10, 22}) {
		t.Fatalf("checkin hours = %v (want sorted deduped)", got.CheckinHours)
	}
	if !reflect.DeepEqual(got.GrowthHours, []int{1}) ||
		!reflect.DeepEqual(got.ActivityHours, []int{12}) ||
		!reflect.DeepEqual(got.TravelHours, []int{8, 20}) ||
		!reflect.DeepEqual(got.BlackcatHours, []int{1, 23}) ||
		got.ProbeMinutes != 3 || got.GoogleProbeMinutes != 20 || got.RefreshMinutes != 90 {
		t.Fatalf("params = %+v", got)
	}
	if v, ok, _ := st.GetSetting(keyCheckinHours); !ok || v != "10,22" {
		t.Fatalf("persisted checkin hours = %q ok=%v", v, ok)
	}
	if v, ok, _ := st.GetSetting(keyRefreshMinutes); !ok || v != "90" {
		t.Fatalf("persisted refresh minutes = %q ok=%v", v, ok)
	}
	if v, ok, _ := st.GetSetting(keyBlackcatHours); !ok || v != "1,23" {
		t.Fatalf("persisted blackcat hours = %q ok=%v", v, ok)
	}
	s2 := testScheduler(t, st)
	s2.LoadSettings()
	restored := s2.TaskScheduleParams()
	if !reflect.DeepEqual(restored, got) {
		t.Fatalf("restored after restart = %+v, want %+v", restored, got)
	}
}

// Validation: empty hour lists, out-of-range hours/minutes are rejected and
// leave the running schedule untouched.
func TestTaskScheduleParamsValidation(t *testing.T) {
	s := testScheduler(t, store.NewMemory())
	before := s.TaskScheduleParams()
	cases := []model.TaskParams{
		{CheckinHours: nil, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: nil, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: nil, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: nil, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: nil, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{24}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{-1}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{24}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 0, GoogleProbeMinutes: 15, RefreshMinutes: 60},
		{CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23}, ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 1441},
	}
	for i, in := range cases {
		if err := s.SetTaskScheduleParams(in); err == nil {
			t.Fatalf("case %d: expected validation error for %+v", i, in)
		}
	}
	if got := s.TaskScheduleParams(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed sets must not touch the schedule: got %+v want %+v", got, before)
	}
	// Boundaries are legal.
	if err := s.SetTaskScheduleParams(model.TaskParams{
		CheckinHours: []int{0, 23}, GrowthHours: []int{23}, ActivityHours: []int{10}, TravelHours: []int{9, 21}, BlackcatHours: []int{23},
		ProbeMinutes: 1, GoogleProbeMinutes: 1440, RefreshMinutes: 1440,
	}); err != nil {
		t.Fatalf("boundary values must pass: %v", err)
	}
}

func TestParseHours(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		ok   bool
	}{
		{"9,21", []int{9, 21}, true},
		{" 21 ，9 ", []int{9, 21}, true}, // fullwidth comma, spaces, unordered
		{"9,9,9", []int{9}, true},
		{"0", []int{0}, true},
		{"abc", nil, false},
		{"24", nil, false},
		{"", nil, false},
	}
	for _, c := range cases {
		got, err := parseHours(c.in)
		if c.ok != (err == nil) {
			t.Fatalf("parseHours(%q) err = %v, want ok=%v", c.in, err, c.ok)
		}
		if c.ok && !reflect.DeepEqual(got, c.want) {
			t.Fatalf("parseHours(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
