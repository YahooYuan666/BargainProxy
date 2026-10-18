package store

import (
	"sort"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/model"
)

// Memory is the in-memory store implementation (tests, first run, and the
// degraded mode when no PostgreSQL is configured).
type Memory struct {
	mu       sync.RWMutex
	accounts map[string]*model.Account
	logs     []model.RequestLog
	tasks    map[string]model.TaskRun // key: account|task|date
	settings map[string]string
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		accounts: map[string]*model.Account{},
		tasks:    map[string]model.TaskRun{},
		settings: map[string]string{},
	}
}

func copyAccount(a *model.Account) *model.Account {
	c := *a.CopyWithCredentials()
	if a.ModelCooldowns != nil {
		c.ModelCooldowns = make(map[string]model.ModelCooldown, len(a.ModelCooldowns))
		for k, v := range a.ModelCooldowns {
			c.ModelCooldowns[k] = v
		}
	}
	if a.Windows != nil {
		c.Windows = make(map[string]model.Window, len(a.Windows))
		for k, v := range a.Windows {
			c.Windows[k] = v
		}
	}
	if a.CostTiers != nil {
		c.CostTiers = make(map[string]model.CostObservation, len(a.CostTiers))
		for k, v := range a.CostTiers {
			c.CostTiers[k] = v
		}
	}
	return &c
}

func (m *Memory) LoadAccounts() ([]*model.Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		out = append(out, copyAccount(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) SaveAccount(acc *model.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[acc.ID] = copyAccount(acc)
	return nil
}

func (m *Memory) DeleteAccount(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.accounts, id)
	return nil
}

func (m *Memory) AppendRequestLog(l *model.RequestLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l.ID = int64(len(m.logs) + 1)
	m.logs = append(m.logs, *l)
	return nil
}

func (m *Memory) RecentRequestLogs(limit int) ([]model.RequestLog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.logs)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]model.RequestLog, 0, limit)
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.logs[i])
	}
	return out, nil
}

func (m *Memory) PurgeRequestLogs(before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.logs[:0]
	removed := 0
	for _, l := range m.logs {
		if l.Time.Before(before) {
			removed++
			continue
		}
		kept = append(kept, l)
	}
	m.logs = kept
	return removed, nil
}

func taskKey(accountID, taskType, runDate string) string {
	return accountID + "|" + taskType + "|" + runDate
}

func (m *Memory) UpsertTaskRun(t *model.TaskRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[taskKey(t.AccountID, t.TaskType, t.RunDate)] = *t
	return nil
}

func (m *Memory) TaskRunFor(accountID, taskType, runDate string) (*model.TaskRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[taskKey(accountID, taskType, runDate)]
	if !ok {
		return nil, ErrNotFound
	}
	return &t, nil
}

func (m *Memory) RecentTaskRuns(limit int) ([]model.TaskRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.TaskRun, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PurgeTaskRuns drops ledger rows that started before the cutoff (the same
// retention window as request logs; keeps the ledger from growing forever).
func (m *Memory) PurgeTaskRuns(before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for k, t := range m.tasks {
		if t.StartedAt.Before(before) {
			delete(m.tasks, k)
			removed++
		}
	}
	return removed, nil
}

func (m *Memory) GetSetting(key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.settings[key]
	return v, ok, nil
}

func (m *Memory) SetSetting(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[key] = value
	return nil
}

func (m *Memory) ListSettings(prefix string) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	for k, v := range m.settings {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out, nil
}

func (m *Memory) Close() error { return nil }
