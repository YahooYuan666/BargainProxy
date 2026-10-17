package opencodezen

import (
	"sync"

	"bargainproxy/internal/store"
)

// newZenMemStore is a thin alias so catalog tests read naturally; the memory
// store already implements the Settings subset the catalog persists through.
func newZenMemStore() store.Store { return store.NewMemory() }

var _ sync.Locker = (sync.Locker)(nil)
