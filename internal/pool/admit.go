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
	adm, _ := p.admissionAt(a, modelName, now)
	return adm
}

// admissionAt is admissionLocked plus the moment the verdict lifts BY ITSELF.
// Callers that answer "nothing is available, retry at X" need that moment:
// reporting now (the old behaviour) told a client to retry immediately into
// the same refusal, which reads as a dead channel rather than a short wait
// (BP-215).
func (p *Pool) admissionAt(a *model.Account, modelName string, now time.Time) (Admission, time.Time) {
	if a == nil {
		return AdmitNone, time.Time{}
	}
	if a.Disabled || a.Paused {
		return AdmitNone, time.Time{}
	}
	if a.Realm == "global" && !p.globalEnabled {
		return AdmitNone, time.Time{}
	}
	grace := p.params.StickyGrace
	if now.Before(a.AccountCooldownUntil) {
		return AdmitNone, a.AccountCooldownUntil
	}
	if mc, ok := a.ModelCooldowns[modelName]; ok && now.Before(mc.Until) {
		return AdmitNone, mc.Until
	}
	// Just out of a rest: let it serve the conversations it already holds, but
	// do not hand it new ones until it proves healthy for a moment.
	if grace > 0 && !a.AccountCooldownUntil.IsZero() {
		if lift := a.AccountCooldownUntil.Add(grace); now.Before(lift) {
			return AdmitSticky, lift
		}
	}
	// Repeated failures without a rest yet: same reasoning, keyed on the
	// breaker counter the soft-rate path already maintains — but BOUNDED IN
	// TIME (BP-215). The counter is cleared only by a successful answer, and a
	// sticky-only account refuses exactly the new conversations that could
	// produce one, so an unbounded band is a deadlock: the account leaves
	// routing for good while the panel still calls it healthy, and a restart
	// does not help because breaker_fails is persisted and re-loaded by Upsert.
	// Field 2026-10-30: opencodezen's breaker reached 2, every request answered
	// "all accounts are cooling down; earliest reset at <now>", and the request
	// never reached upstream. Once the failures are older than the grace the
	// account is offered new conversations again; a fresh failure re-arms it.
	if p.params.StickyFailThreshold > 0 && grace > 0 && a.BreakerFails >= p.params.StickyFailThreshold {
		if lift := a.UpdatedAt.Add(grace); now.Before(lift) {
			return AdmitSticky, lift
		}
	}
	return AdmitNew, time.Time{}
}
