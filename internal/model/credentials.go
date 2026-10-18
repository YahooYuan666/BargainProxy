package model

import (
	"bytes"
	"sync"
)

var credentialsMu sync.RWMutex

func (a *Account) Credentials() []byte {
	credentialsMu.RLock()
	defer credentialsMu.RUnlock()
	return append([]byte(nil), a.CredsJSON...)
}

func (a *Account) SetCredentials(raw []byte) {
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	a.CredsJSON = append([]byte(nil), raw...)
	a.CredsDirty = true
}

func (a *Account) CredentialsDirty() bool {
	credentialsMu.RLock()
	defer credentialsMu.RUnlock()
	return a.CredsDirty
}

func (a *Account) CredentialsPersisted(raw []byte) {
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	if bytes.Equal(a.CredsJSON, raw) {
		a.CredsDirty = false
	}
}

func (a *Account) CopyWithCredentials() *Account {
	credentialsMu.RLock()
	defer credentialsMu.RUnlock()
	c := *a
	c.CredsJSON = append([]byte(nil), a.CredsJSON...)
	return &c
}
