// Scheduler-side OpenCode Zen automation: one hook the task loop calls every
// minute. Only the catalog sync (with gateway-route + ZCode config
// application) runs on the panel-editable cadence (default 60 min).
//
// There is deliberately no scheduled probe. Every probe is a real chat call on
// the anonymous free lane, so a periodic pass both spends the shared quota and
// turns any transient 429 into a page full of "unavailable" models. The live
// availability pass runs once at boot and otherwise only when the user asks
// for it from the panel.
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
	s.mu.Unlock()
}

func (s *Scheduler) zenTick(now time.Time) {
	s.mu.Lock()
	z := s.zen
	minutes := s.zenSyncMinutes
	lastSync := s.zenLastSync
	s.mu.Unlock()
	if z == nil || minutes <= 0 {
		return
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
