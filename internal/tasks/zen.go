// Scheduler-side OpenCode Zen automation: one hook the task loop calls every
// minute. The catalog sync (with gateway-route + ZCode config application)
// runs on the panel-editable cadence (default 60 min); a live probe runs at
// half that cadence so availability tracks reality, and every probe feeds a
// sync so the ZCode picker follows within minutes of a model dying/recovering.
package tasks

import (
	"context"
	"time"
)

// ZenRunner is the OpenCode Zen automation surface (implemented by the panel
// via a small adapter; nil = feature off).
type ZenRunner interface {
	// ZenAutoTick runs one automatic sync cycle (catalog + downstream apply)
	// and reports whether it ran.
	ZenAutoTick(ctx context.Context) bool
	// ZenProbeTick probes the candidate set (skips when none) and reports
	// whether it ran.
	ZenProbeTick(ctx context.Context) bool
}

// SetZenSyncMinutes hot-updates the sync cadence (panel-editable).
func (s *Scheduler) SetZenSyncMinutes(n int) {
	s.mu.Lock()
	s.zenSyncMinutes = n
	s.mu.Unlock()
}

// SetZenRunner wires the automation hook (called from main).
func (s *Scheduler) SetZenRunner(z ZenRunner) {
	s.mu.Lock()
	s.zen = z
	s.zenLastSync = time.Time{} // recompute cadence from now
	s.zenLastProbe = time.Time{}
	s.mu.Unlock()
}

func (s *Scheduler) zenTick(now time.Time) {
	s.mu.Lock()
	z := s.zen
	minutes := s.zenSyncMinutes
	lastSync, lastProbe := s.zenLastSync, s.zenLastProbe
	s.mu.Unlock()
	if z == nil || minutes <= 0 {
		return
	}
	// Probe first at half the sync cadence (bounded 15..720 min), then sync so
	// fresh probe results land in the ZCode config in the same pass.
	if now.Sub(lastProbe) >= time.Duration(minutes/2)*time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		if z.ZenProbeTick(ctx) {
			s.mu.Lock()
			s.zenLastProbe = now
			s.mu.Unlock()
		}
		cancel()
	}
	if now.Sub(lastSync) >= time.Duration(minutes)*time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if z.ZenAutoTick(ctx) {
			s.mu.Lock()
			s.zenLastSync = now
			s.mu.Unlock()
		}
		cancel()
	}
}
