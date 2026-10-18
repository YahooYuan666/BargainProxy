package store

import (
	"context"
	"encoding/json"
	"time"

	"bargainproxy/internal/model"
)

func (m *Memory) SaveAccountState(acc *model.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.accounts[acc.ID]
	if old == nil {
		return nil
	}
	if acc.UpdatedAt.Before(old.UpdatedAt) {
		return nil
	}
	c := copyAccount(acc)
	c.CredsJSON = append([]byte(nil), old.CredsJSON...)
	m.accounts[acc.ID] = c
	return nil
}

func (p *Postgres) SaveAccountState(acc *model.Account) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sb := stateBlob{Paused: acc.Paused, AccountCooldownUntil: acc.AccountCooldownUntil, ModelCooldowns: acc.ModelCooldowns, Windows: acc.Windows, Credits: acc.Credits, CostTiers: acc.CostTiers, ConsecutiveFails: acc.ConsecutiveFails, BreakerFails: acc.BreakerFails, LastUsedAt: acc.LastUsedAt, UpdatedAt: acc.UpdatedAt}
	raw, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `UPDATE accounts SET disabled=$2, state=$3, updated_at=now() WHERE id=$1 AND (COALESCE(state->>'updated_at','')='' OR (state->>'updated_at')::timestamptz <= $4)`, acc.ID, acc.Disabled, raw, acc.UpdatedAt)
	return err
}
