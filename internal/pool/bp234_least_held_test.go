package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// BP-234 (magpie 06d36d41): a NEW conversation spreads across the pool —
// among the ready candidates it lands on the account currently holding the
// fewest live session bindings. The scenario is the credits-weighted one:
// a rich account wins every strategy pick, and without the spread factor
// new sessions pile onto it (the 272.6k-input hotspot, BP-66). The sticky
// bindings themselves are untouched.
func TestBP234NewSessionsSpreadOverLeastHeld(t *testing.T) {
	// Ten fresh pools, the rich account pre-bound to one live session: with
	// the spread, a NEW session lands on the least-held poor account every
	// single time (without it the credits-weighted draw keeps pulling onto
	// wb-rich).
	for i := 0; i < 10; i++ {
		p := New(Params{MaxQueueWait: time.Second},
			map[string]int{model.ChannelWorkBuddy: 2}, nil)
		rich := mkAccount("wb-rich", model.ChannelWorkBuddy)
		rich.Credits = model.Credits{Remain: 1_000_000, Total: 1_000_000}
		p.Upsert(rich)
		p.Upsert(mkAccount("wb-poor", model.ChannelWorkBuddy))
		p.sessions["workbuddy|sess-1"] = sessionBinding{accountID: "wb-rich", lastUsed: time.Now()}

		l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-2")
		if err != nil {
			t.Fatalf("pool %d acquire: %v", i, err)
		}
		l2.Release()
		if l2.Account.ID != "wb-poor" {
			t.Fatalf("pool %d: a new session must spread to the least-held account, landed on %s", i, l2.Account.ID)
		}
	}
}

// The spread must NOT break stickiness: an already-bound conversation keeps
// its account even when it holds more sessions than the other.
func TestBP234StickinessUnbroken(t *testing.T) {
	pinDraw(t, 0) // deterministic: the highest-weighted candidate wins
	p := New(Params{MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	rich := mkAccount("wb-rich", model.ChannelWorkBuddy)
	rich.Credits = model.Credits{Remain: 1_000_000, Total: 1_000_000}
	p.Upsert(rich)
	p.Upsert(mkAccount("wb-poor", model.ChannelWorkBuddy))

	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if l1.Account.ID != "wb-rich" {
		l1.Release()
		t.Fatalf("first session = %s, want wb-rich", l1.Account.ID)
	}
	l1.Release()

	p1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "sess-1")
	if err != nil {
		t.Fatalf("sticky re-acquire: %v", err)
	}
	if p1.Account.ID != "wb-rich" {
		p1.Release()
		t.Fatalf("sticky session moved to %s", p1.Account.ID)
	}
	p1.Release()
}

// The direct gate: with two live bindings held by wb-rich, only wb-poor
// survives the least-held band; equal densities pass through untouched, and
// a stale binding does not count as held.
func TestBP234LeastHeldGateUnit(t *testing.T) {
	p := New(Params{}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	rich := mkAccount("wb-rich", model.ChannelWorkBuddy)
	poor := mkAccount("wb-poor", model.ChannelWorkBuddy)
	now := time.Now()
	p.sessions["workbuddy|s1"] = sessionBinding{accountID: "wb-rich", lastUsed: now}
	p.sessions["workbuddy|s2"] = sessionBinding{accountID: "wb-rich", lastUsed: now}

	out := p.leastHeldLocked("workbuddy", []*model.Account{rich, poor}, now)
	if len(out) != 1 || out[0].ID != "wb-poor" {
		t.Fatalf("least-held band = %v, want only wb-poor", out)
	}

	// A binding outside the live window does not count as held.
	p.sessions["workbuddy|s-old"] = sessionBinding{accountID: "wb-rich", lastUsed: now.Add(-2 * time.Hour)}
	if got := p.sessionsHeldLocked("workbuddy", "wb-rich", now); got != 2 {
		t.Fatalf("sessionsHeld = %d, want 2 (the stale binding not counted)", got)
	}

	// Equal densities pass through untouched.
	p2 := New(Params{}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	if out := p2.leastHeldLocked("workbuddy", []*model.Account{rich, poor}, now); len(out) != 2 {
		t.Fatalf("equal densities must pass through, got %d candidates", len(out))
	}
}

// pinDraw forces the weighted draw to a fixed value for one test, so an
// assertion about WHICH account wins is deterministic instead of a coin flip
// (BP-255). 0 means "take the first candidate in weight order".
func pinDraw(t *testing.T, v float64) {
	t.Helper()
	prev := randFloat64
	randFloat64 = func() float64 { return v }
	t.Cleanup(func() { randFloat64 = prev })
}

// Antigravity window pools must NOT be sliced by leastHeld: burn-rate greed
// takes precedence so accounts near reset are exhausted before they expire,
// even if they already hold a live session.
func TestBP234AntigravityExemptFromLeastHeld(t *testing.T) {
	now := time.Date(2026, 11, 1, 19, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelAntigravity: 3}, nil)

	urgent := mkAccount("ag-urgent", model.ChannelAntigravity)
	urgent.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.83, ResetAt: now.Add(4 * time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.85, ResetAt: now.Add(4 * time.Hour), UpdatedAt: now},
	}
	fresh := mkAccount("ag-fresh", model.ChannelAntigravity)
	fresh.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.95, ResetAt: now.Add(4 * 24 * time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.92, ResetAt: now.Add(4 * time.Hour), UpdatedAt: now},
	}
	p.Upsert(urgent)
	p.Upsert(fresh)

	// urgent account already holds one live session
	p.sessions["antigravity|sess-1"] = sessionBinding{accountID: "ag-urgent", lastUsed: now}

	// A new session (sess-2) arrives: leastHeld would have sliced away urgent
	// and picked fresh. But Antigravity must stay greedy and pick urgent.
	lease, err := p.Acquire(context.Background(), model.ChannelAntigravity, "gemini-3.8-flash-high", "sess-2")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "ag-urgent" {
		t.Fatalf("Antigravity must exempt from leastHeld and pick greedy ag-urgent, got %s", lease.Account.ID)
	}
}

