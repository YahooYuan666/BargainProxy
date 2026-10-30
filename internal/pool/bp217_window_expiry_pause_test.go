package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// BP-217: a first-byte window expiry means OUR timer gave up on an upstream
// that sent zero bytes. One stall is a slow response; TWO on the same
// channel+model inside the span is a silently hung upstream — and without a
// pause every later request re-paid the full window on the dead leg (field
// 2026-10-30 22:43–23:03: 17 requests × 60s on a zero-byte workbuddy before
// the trae cascade). Two expiries trip the shared pause; the account state
// machine stays untouched (ticket 27).
func TestBP217SecondWindowExpiryPausesThePair(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 43, 2, 0, time.UTC)
	p := New(Params{
		Now:               func() time.Time { return now },
		WindowExpiryPause: 2 * time.Minute,
	}, nil, nil)
	a := mkAccount("wb-217", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.NoteWindowExpiry(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "deepseek-v4.1-flash", now); ok {
		t.Fatal("ONE window expiry paused the pair — a single slow response must never pause anything")
	}

	now = now.Add(time.Minute)
	p.NoteWindowExpiry(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	until, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "deepseek-v4.1-flash", now)
	if !ok {
		t.Fatal("two window expiries inside the span did not pause the pair — every later request would re-pay the window on the hung leg")
	}
	if wait := until.Sub(now); wait > 2*time.Minute {
		t.Fatalf("pause = %s, want the configured %s", wait, 2*time.Minute)
	}
	// The pause is scoped to the channel+model pair: the same channel serving
	// another model, and another channel serving the same model, stay live.
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "glm-5.3", now); ok {
		t.Fatal("a different model on the paused channel must not inherit the pause")
	}
	if _, ok := p.ChannelBusyUntil(model.ChannelTrae, "deepseek-v4.1-flash", now); ok {
		t.Fatal("a different channel must not inherit the pause")
	}
	// Ticket 27: the cancellation class carries no account penalty — the
	// pause is channel-scoped shared state, not a strike on the account.
	if !a.AccountCooldownUntil.IsZero() || a.ConsecutiveFails != 0 {
		t.Fatalf("window expiries touched the account state (cooldown=%v fails=%d)",
			a.AccountCooldownUntil, a.ConsecutiveFails)
	}
}

// An answer between two stalls means the leg recovered — stall → success →
// stall is two isolated slow responses, not a hung upstream.
func TestBP217SuccessClearsThePileUp(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 43, 2, 0, time.UTC)
	p := New(Params{
		Now:               func() time.Time { return now },
		WindowExpiryPause: 2 * time.Minute,
	}, nil, nil)
	a := mkAccount("wb-217", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.NoteWindowExpiry(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	now = now.Add(time.Minute)
	p.ApplyResult(a, "deepseek-v4.1-flash", nil) // the leg answered
	now = now.Add(time.Minute)
	p.NoteWindowExpiry(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "deepseek-v4.1-flash", now); ok {
		t.Fatal("stall → success → stall tripped the pause — a success must clear the pile-up")
	}
}

// Expiries older than the span are forgotten: two stalls eleven minutes
// apart say "slow now and then", not "hung".
func TestBP217ExpiriesOutsideTheSpanDoNotTrip(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 43, 2, 0, time.UTC)
	p := New(Params{
		Now:               func() time.Time { return now },
		WindowExpiryPause: 2 * time.Minute,
	}, nil, nil)

	p.NoteWindowExpiry(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	now = now.Add(11 * time.Minute)
	p.NoteWindowExpiry(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "deepseek-v4.1-flash", now); ok {
		t.Fatal("two expiries outside the span tripped the pause")
	}
}
