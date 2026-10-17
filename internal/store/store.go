// Package store defines the persistence contract and its implementations.
//
// Contract summary:
//   - Accounts keep their scheduling state persisted EXCEPT volatile in-flight
//     counters, which are memory-only so a crash can never leave an account
//     permanently "busy".
//   - Credential blobs go through the crypto envelope; the store layer never
//     sees raw plaintext on disk.
//   - All SQL uses parameter binding (no string-built statements).
package store

import (
	"errors"
	"time"

	"bargainproxy/internal/model"
)

// ErrNotFound is returned by point lookups that miss.
var ErrNotFound = errors.New("store: not found")

// Store is the full persistence contract.
type Store interface {
	// Accounts
	LoadAccounts() ([]*model.Account, error)
	SaveAccount(acc *model.Account) error
	DeleteAccount(id string) error

	// Request audit log (metadata only)
	AppendRequestLog(l *model.RequestLog) error
	RecentRequestLogs(limit int) ([]model.RequestLog, error)
	PurgeRequestLogs(before time.Time) (int, error)

	// Task executions
	UpsertTaskRun(t *model.TaskRun) error
	TaskRunFor(accountID, taskType, runDate string) (*model.TaskRun, error)
	RecentTaskRuns(limit int) ([]model.TaskRun, error)
	PurgeTaskRuns(before time.Time) (int, error)

	// Settings
	GetSetting(key string) (string, bool, error)
	SetSetting(key, value string) error
	// ListSettings returns every setting whose key starts with prefix. Used by
	// the panel to reload persisted pending login flows at startup (BP-51).
	ListSettings(prefix string) (map[string]string, error)

	Close() error
}
