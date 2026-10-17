package tasks

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

func testScheduler(t *testing.T, st store.Store) *Scheduler {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{}, st)
	return New(gw, st, p, log.New(io.Discard, "", 0))
}

// TestDisabledTaskSkipsScheduledButNotManual: the per-task switch gates timer
// triggers only — an explicit manual run is never blocked.
func TestDisabledTaskSkipsScheduledButNotManual(t *testing.T) {
	st := store.NewMemory()
	s := testScheduler(t, st)
	now := time.Now()

	// Defaults: everything scheduled.
	if !s.runScheduled(TaskCheckinWorkBuddy, now) {
		t.Fatal("task must be scheduled by default")
	}

	if err := s.SetTaskEnabled(TaskProbeAll, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	// The switch must persist for restart recovery.
	if v, ok, _ := st.GetSetting(TaskEnabledKey(TaskProbeAll)); !ok || v != "false" {
		t.Fatalf("persisted switch = %q ok=%v", v, ok)
	}
	if s.TaskEnabled(TaskProbeAll) {
		t.Fatal("switch must read back disabled")
	}
	if s.runScheduled(TaskProbeAll, now) {
		t.Fatal("disabled task must be skipped by the timer")
	}

	// Manual runs ignore the switch (explicit intent).
	if _, err := s.RunTask(context.Background(), TaskProbeAll, ""); err != nil {
		t.Fatalf("manual run must not be gated: %v", err)
	}

	// Re-enable and the timer path runs again.
	if err := s.SetTaskEnabled(TaskProbeAll, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !s.runScheduled(TaskProbeAll, now) {
		t.Fatal("re-enabled task must be scheduled again")
	}

	// Unknown task types are rejected.
	if err := s.SetTaskEnabled("no_such_task", false); err == nil {
		t.Fatal("unknown task type must be rejected")
	}
}

// TestLoadSettingsRestoresSwitches: persisted switches survive a restart.
func TestLoadSettingsRestoresSwitches(t *testing.T) {
	st := store.NewMemory()
	if err := st.SetSetting(TaskEnabledKey(TaskGrowthWorkBuddy), "false"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := testScheduler(t, st)
	s.LoadSettings()
	states := s.TaskEnabledStates()
	if states[TaskGrowthWorkBuddy] {
		t.Fatalf("growth switch must be restored off: %+v", states)
	}
	if !states[TaskCheckinTrae] {
		t.Fatalf("unset switches default on: %+v", states)
	}
}
