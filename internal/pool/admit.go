package pool

import (
	"time"

	"bargainproxy/internal/model"
)

// Admission is how much new traffic an account may take right now.
//
// Two questions are easy to conflate and expensive to conflate wrongly:
// "may a conversation that is already bound here continue?" and "may a NEW
// conversation be placed here?". An account that just came back from a short
// rest is a poor home for new conversations (it may rest again) but an
// excellent home for the conversation it already holds — moving that
// conversation would lose its prompt cache and its context affinity. The
// reference gateways model exactly this with three states (sub2api's
// Schedulable / StickyOnly / NotSchedulable, 2026-10-22).
type Admission string

const (
	// AdmitNew: the account may take new conversations and continue bound ones.
	AdmitNew Admission = "schedulable"
	// AdmitSticky: only a conversation already bound to this account may use
	// it. New conversations go elsewhere.
	AdmitSticky Admission = "sticky_only"
	// AdmitNone: nothing may use it.
	AdmitNone Admission = "not_schedulable"
)

// AdmissionOf classifies one (account, model) pair for callers outside the
// pool (panel views, probes, tests).
func (p *Pool) AdmissionOf(a *model.Account, modelName string) Admission {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.admissionLocked(a, modelName, p.params.Now())
}

// admissionLocked classifies an account for one model. Callers must hold the
// lock.
//
// The StickyOnly band is deliberately narrow: it covers an account whose rest
// has just ended (a short grace window) or whose last N calls failed but which
// is not yet resting. Both are "probably fine for the conversation that knows
// it, probably not fine for a stranger". Anything harder (disabled, paused,
// realm-hidden, actually cooling) is AdmitNone.
func (p *Pool) admissionLocked(a *model.Account, modelName string, now time.Time) Admission {
	if a == nil {
		return AdmitNone
	}
	if a.Disabled || a.Paused {
		return AdmitNone
	}
	if a.Realm == "global" && !p.globalEnabled {
		return AdmitNone
	}
	if now.Before(a.AccountCooldownUntil) {
		return AdmitNone
	}
	if mc, ok := a.ModelCooldowns[modelName]; ok && now.Before(mc.Until) {
		return AdmitNone
	}
	// Just out of a rest: let it serve the conversations it already holds, but
	// do not hand it new ones until it proves healthy for a moment.
	if grace := p.params.StickyGrace; grace > 0 && !a.AccountCooldownUntil.IsZero() {
		if now.Sub(a.AccountCooldownUntil) < grace {
			return AdmitSticky
		}
	}
	// Repeated failures without a rest yet: same reasoning, keyed on the
	// breaker counter the soft-rate path already maintains.
	if p.params.StickyFailThreshold > 0 && a.BreakerFails >= p.params.StickyFailThreshold {
		return AdmitSticky
	}
	return AdmitNew
}
