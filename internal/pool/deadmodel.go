package pool

import (
	"time"
)

// Dead-model detection.
//
// A model can disappear upstream while the gateway's route table still lists
// it. Re-probing it on every account turns one dead model into a storm of
// useless upstream calls and a pool that looks broken. The reference gateways
// promote that to a single fact: when at least two DIFFERENT accounts of the
// same channel are explicitly told by the upstream that it has no such model,
// the model is treated as unavailable for that channel and is skipped before
// any account is touched (magpie internal/pool/deadmodels.go, 2026-10-21).
//
// Two boundaries matter and are enforced here:
//   - positive evidence only: a model merely MISSING from a remotely fetched
//     catalogue is never enough (a degraded fetch, a plan mismatch or a cold
//     cache all look alike), so only an explicit "no such model" answer counts;
//   - distinct accounts: one account answering ten times is one account's
//     problem (its session may be routed to a cluster without the model), so
//     the threshold counts uids, not attempts.
//
// The record expires so a re-listed model comes back without a restart.

const (
	// deadModelAccounts is how many DISTINCT accounts must report "no such
	// model" before the channel+model pair is treated as gone.
	deadModelAccounts = 2
	// deadModelTTL is how long that verdict holds before the pair is probed
	// again. Long enough to stop the storm, short enough that a restored
	// model returns on its own.
	deadModelTTL = 6 * time.Hour
)

// deadModelVerdict is one channel+model suspected of being gone.
type deadModelVerdict struct {
	accounts map[string]time.Time // account uid/id -> first report
	decided  time.Time            // when the threshold was reached (zero = not yet)
	reason   string               // upstream words from the last report
}

// DeadModel is the panel-facing snapshot of a dead-model verdict.
type DeadModel struct {
	Channel  string    `json:"channel"`
	Model    string    `json:"model"`
	Accounts int       `json:"accounts"`
	Reason   string    `json:"reason,omitempty"`
	Decided  time.Time `json:"decided"`
	Until    time.Time `json:"until"`
}

func deadKey(channel, modelName string) string { return channel + "|" + modelName }

// noteMissingModel records one account's explicit "no such model" answer.
// Callers must hold the lock.
func (p *Pool) noteMissingModelLocked(channel, modelName, accountID, reason string, now time.Time) {
	if channel == "" || modelName == "" || accountID == "" {
		return
	}
	if p.deadModels == nil {
		p.deadModels = map[string]*deadModelVerdict{}
	}
	k := deadKey(channel, modelName)
	v := p.deadModels[k]
	if v == nil {
		v = &deadModelVerdict{accounts: map[string]time.Time{}}
		p.deadModels[k] = v
	}
	if _, seen := v.accounts[accountID]; !seen {
		v.accounts[accountID] = now
	}
	v.reason = reason
	// Decide only once; later reports keep refreshing the reason text.
	if v.decided.IsZero() && len(v.accounts) >= deadModelAccounts {
		v.decided = now
	}
}

// deadModelLocked reports whether channel+model is currently treated as gone.
// Callers must hold the lock.
func (p *Pool) deadModelLocked(channel, modelName string, now time.Time) (bool, string, int) {
	if p.deadModels == nil {
		return false, "", 0
	}
	v := p.deadModels[deadKey(channel, modelName)]
	if v == nil || v.decided.IsZero() {
		return false, "", 0
	}
	if now.Sub(v.decided) >= deadModelTTL {
		// Expired: clear it so the pair is probed again.
		delete(p.deadModels, deadKey(channel, modelName))
		return false, "", 0
	}
	return true, v.reason, len(v.accounts)
}

// ForgetDeadModel clears a verdict (panel "restore" action, or a successful
// call proving the model is back).
func (p *Pool) ForgetDeadModel(channel, modelName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.deadModels, deadKey(channel, modelName))
}

// DeadModels lists the live verdicts for the panel.
func (p *Pool) DeadModels() []DeadModel {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	out := make([]DeadModel, 0, len(p.deadModels))
	for k, v := range p.deadModels {
		if v.decided.IsZero() || now.Sub(v.decided) >= deadModelTTL {
			continue
		}
		ch, mdl := splitDeadKey(k)
		out = append(out, DeadModel{
			Channel: ch, Model: mdl, Accounts: len(v.accounts),
			Reason: v.reason, Decided: v.decided, Until: v.decided.Add(deadModelTTL),
		})
	}
	return out
}

func splitDeadKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
