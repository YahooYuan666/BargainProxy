// BP-44: the experimental real-thought-signature replay toggle is wired from
// the gateway down to the antigravity driver.
package gateway

import (
	"sync/atomic"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/model"
)

// stubSigDriver embeds the Driver interface (unused methods panic if called)
// and records the toggle.
type stubSigDriver struct {
	driver.Driver
	sig atomic.Bool
}

func (s *stubSigDriver) SetThoughtSignatureReplay(v bool) { s.sig.Store(v) }

func TestThoughtSignatureReplaySetterReachesDriver(t *testing.T) {
	stub := &stubSigDriver{}
	g := &Gateway{drivers: map[string]driver.Driver{model.ChannelAntigravity: stub}}
	g.SetThoughtSignatureReplay(true)
	if !stub.sig.Load() {
		t.Fatal("setter did not reach the antigravity driver")
	}
	g.SetThoughtSignatureReplay(false)
	if stub.sig.Load() {
		t.Fatal("setter did not turn the toggle off")
	}
}

// A driver without the capability must not panic the setter.
func TestThoughtSignatureReplaySetterToleratesPlainDriver(t *testing.T) {
	g := &Gateway{drivers: map[string]driver.Driver{model.ChannelAntigravity: &stubSigDriver{}}}
	g.drivers[model.ChannelAntigravity] = struct{ driver.Driver }{}
	g.SetThoughtSignatureReplay(true) // no panic = pass
}
