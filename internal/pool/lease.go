package pool

import (
	"bargainproxy/internal/model"
)

// Lease is a reserved in-flight slot on one account. The caller must Release
// it exactly once (defer). Account fields may be read freely; scheduling
// fields are mutated through the Pool methods so every change flows through
// the state machine and persistence callback.
type Lease struct {
	Account  *model.Account
	pool     *Pool
	released bool
}

// Release returns the slot to the pool and wakes any waiters.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.pool.Release(l)
}

// Pool returns the owning pool (used by drivers that need to record quota).
func (l *Lease) Pool() *Pool { return l.pool }
