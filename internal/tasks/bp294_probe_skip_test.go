package tasks

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// BP-294: a RESTING account is upstream traffic with no upside — the pool
// will not select it until its cooldown lifts, so probing it only teaches the
// panel what it already knows. This is the "second path" that kept a
// TOS-banned Google account polled every google_probe_minutes after BP-292
// fixed the request path: the quota endpoint keeps answering 200 for a
// suspended account, so the probe never failed, never marked anything, and
// ran ~96 times a day against credentials Google had just revoked service
// for (magpie hit the identical hole, yetone/magpie issue #5478).
//
// Automatic runs skip cooling accounts the same way they skip Disabled ones;
// an account-targeted run (the panel's manual probe button) always probes —
// an operator clicking it explicitly wants the fresh answer.

const bp294Channel = model.ChannelAntigravity

type fakeProberDriver struct {
	mu     sync.Mutex
	probes []string
}

func (f *fakeProberDriver) Channel() string { return bp294Channel }

func (f *fakeProberDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	return nil, uperr.New(uperr.ClassInternal, 0, "BP-294 fake: Stream not used")
}

func (f *fakeProberDriver) Models(ctx context.Context) ([]string, error) { return nil, nil }

func (f *fakeProberDriver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	f.mu.Lock()
	f.probes = append(f.probes, acc.ID)
	f.mu.Unlock()
	return &driver.ProbeResult{Note: "ok"}, nil
}

func (f *fakeProberDriver) probed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.probes...)
}

func buildProbeScheduler(t *testing.T) (*Scheduler, *fakeProberDriver) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{bp294Channel: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	fd := &fakeProberDriver{}
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{bp294Channel: fd}, st)
	sched := New(gw, st, p, nil)
	sched.accountSpread = 0 // no spread sleeps under test

	healthy := &model.Account{ID: bp294Channel + ":healthy", Channel: bp294Channel, Nickname: "healthy"}
	banned := &model.Account{
		ID: bp294Channel + ":banned", Channel: bp294Channel, Nickname: "banned",
		AccountCooldownUntil:  time.Now().Add(7 * 24 * time.Hour),
		AccountCooldownReason: "restricted by upstream",
	}
	expired := &model.Account{
		ID: bp294Channel + ":expired", Channel: bp294Channel, Nickname: "expired",
		AccountCooldownUntil: time.Now().Add(-time.Minute), // rest already over
	}
	p.Upsert(healthy)
	p.Upsert(banned)
	p.Upsert(expired)
	return sched, fd
}

// Automatic probe runs must leave resting accounts alone.
func TestBP294ProbeSkipsCoolingAccounts(t *testing.T) {
	sched, fd := buildProbeScheduler(t)
	detail, err := sched.probeAll(context.Background(), "")
	if err != nil {
		t.Fatalf("probeAll: %v", err)
	}
	got := fd.probed()
	if len(got) != 2 {
		t.Fatalf("probed = %v, want exactly the non-resting accounts (healthy, expired)", got)
	}
	for _, id := range got {
		if id == bp294Channel+":banned" {
			t.Fatalf("resting account %s was probed anyway", id)
		}
	}
	if !contains(got, bp294Channel+":healthy") || !contains(got, bp294Channel+":expired") {
		t.Fatalf("probed = %v, want healthy and expired both probed", got)
	}
	if !strings.Contains(detail, "resting skipped") {
		t.Fatalf("detail = %q, want the resting-skip count surfaced", detail)
	}
}

// A manual, account-targeted run is an operator's explicit ask: it probes the
// resting account too (the panel button must keep working on a banned
// account, e.g. to watch for an appeal being granted).
func TestBP294ManualProbeStillReachesRestingAccount(t *testing.T) {
	sched, fd := buildProbeScheduler(t)
	if _, err := sched.probeAll(context.Background(), bp294Channel+":banned"); err != nil {
		t.Fatalf("manual probeAll: %v", err)
	}
	if got := fd.probed(); !contains(got, bp294Channel+":banned") {
		t.Fatalf("probed = %v, want the manually targeted resting account", got)
	}
}

// A rest that has lifted returns the account to the automatic rotation — the
// skip must not be a life sentence (BP-205: restricted accounts come back on
// their own after the rest).
func TestBP294ExpiredRestIsProbedAgain(t *testing.T) {
	sched, fd := buildProbeScheduler(t)
	if _, err := sched.probeAll(context.Background(), ""); err != nil {
		t.Fatalf("probeAll: %v", err)
	}
	if got := fd.probed(); !contains(got, bp294Channel+":expired") {
		t.Fatalf("probed = %v, want the account whose rest already ended", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
