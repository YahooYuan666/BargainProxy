package pool

import "bargainproxy/internal/model"

// stateSnapshot excludes credentials: delayed scheduling updates must never
// overwrite a token that rotated while the update was queued.
func stateSnapshot(a *model.Account) *model.Account {
	c := &model.Account{
		ID: a.ID, Channel: a.Channel, UID: a.UID, Nickname: a.Nickname,
		Realm: a.Realm, ProjectID: a.ProjectID,
		Disabled: a.Disabled, Paused: a.Paused,
		AccountCooldownUntil: a.AccountCooldownUntil, Credits: a.Credits,
		ConsecutiveFails: a.ConsecutiveFails, BreakerFails: a.BreakerFails,
		LastUsedAt: a.LastUsedAt, UpdatedAt: a.UpdatedAt,
	}
	c.ModelCooldowns = make(map[string]model.ModelCooldown, len(a.ModelCooldowns))
	for k, v := range a.ModelCooldowns {
		c.ModelCooldowns[k] = v
	}
	c.Windows = make(map[string]model.Window, len(a.Windows))
	for k, v := range a.Windows {
		c.Windows[k] = v
	}
	c.CostTiers = make(map[string]model.CostObservation, len(a.CostTiers))
	for k, v := range a.CostTiers {
		c.CostTiers[k] = v
	}
	return c
}

func (p *Pool) changedLocked(a *model.Account) {
	if p.onChange == nil || p.closed || p.accounts[a.ID] != a {
		return
	}
	a.UpdatedAt = p.params.Now()
	p.pending[a.ID] = stateSnapshot(a)
	select {
	case p.persistWake <- struct{}{}:
	default:
	}
}

func (p *Pool) persistLoop() {
	defer close(p.persistDone)
	for range p.persistWake {
		for {
			p.mu.Lock()
			var snapshot *model.Account
			for id, a := range p.pending {
				snapshot = a
				delete(p.pending, id)
				break
			}
			closed := p.closed
			p.mu.Unlock()
			if snapshot == nil {
				if closed {
					return
				}
				break
			}
			p.onChange(snapshot)
		}
	}
}

// Close drains accepted state updates. Call after request/task producers stop
// and before closing the underlying store.
func (p *Pool) Close() {
	if p.onChange == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	select {
	case p.persistWake <- struct{}{}:
	default:
	}
	<-p.persistDone
}
