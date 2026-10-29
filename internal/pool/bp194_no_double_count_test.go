package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-194 (magpie 87c68c9): a failure reported for an account that is ALREADY
// resting must not raise its backoff ladders again — one request walking its
// retries through the same account pushed seven upstream accounts to the
// 10-minute cap in a single turn.
func TestBP194RestingAccountDoesNotClimbTheLadder(t *testing.T) {
	cur := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }}, nil, nil)
	a := mkAccount("a-194", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate"))
	first := a.BreakerFails
	if first == 0 {
		t.Fatal("the first failure must count")
	}

	// The same request's retry against the now-resting account: no second
	// increment, the ladder stays where the first failure put it.
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate"))
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate"))
	if a.BreakerFails != first {
		t.Fatalf("retries against a resting account climbed the ladder: %d -> %d", first, a.BreakerFails)
	}

	// A success clears the counters; with the rest EXPIRED the account is
	// serving again and the next failure counts from one.
	cur = cur.Add(20 * time.Minute) // past the soft-rate rest
	p.ApplyResult(a, "glm-5.3", nil)
	if a.BreakerFails != 0 || a.ConsecutiveFails != 0 {
		t.Fatalf("success must clear the counters, got %d/%d", a.BreakerFails, a.ConsecutiveFails)
	}
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassSoftRate, 429, "rate"))
	if a.BreakerFails != 1 {
		t.Fatalf("a failure on a serving account must count, got %d", a.BreakerFails)
	}
}
