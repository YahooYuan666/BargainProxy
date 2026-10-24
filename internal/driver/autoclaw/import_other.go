//go:build !windows

package autoclaw

import "errors"

// dpapiUnprotect is Windows-only: the client's credential store is protected by
// DPAPI under the logged-in Windows user, and there is no portable equivalent.
func dpapiUnprotect([]byte) ([]byte, error) {
	return nil, errors.New("reading the official client's credentials requires Windows (DPAPI)")
}
