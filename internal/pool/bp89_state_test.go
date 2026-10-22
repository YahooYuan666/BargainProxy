package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestCooldownCarriesReasonAndBasis: an operator must be able to see WHY an
// account rests and what decided the deadline, not just that it is "cooling
// down" (2026-10-22, magpie's Rest.Why/By split).
func TestCooldownCarriesReasonAndBasis(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	p.Upsert(a)

	reset := now.Add(45 * time.Minute)
	p.ApplyResult(a, "glm-5.3", &uperr.Error{Class: uperr.ClassSoftRate, ResetAt: reset, Message: "slow down"})

	if a.AccountCooldownReason != "rate limit" {
		t.Fatalf("reason = %q, want rate limit", a.AccountCooldownReason)
	}
	if a.AccountCooldownBy != "upstream" {
		t.Fatalf("by = %q, want upstream (the vendor stated the reset)", a.AccountCooldownBy)
	}

	// A transport failure is a backoff, not an upstream-stated moment.
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(b)
	p.ApplyResult(b, "glm-5.3", &uperr.Error{Class: uperr.ClassTransport, Message: "dial tcp: refused"})
	if b.AccountCooldownReason != "transport error" || b.AccountCooldownBy != "backoff" {
		t.Fatalf("transport rest = %q/%q", b.AccountCooldownReason, b.AccountCooldownBy)
	}

	// A successful call after the rest expired clears the explanation.
	a.AccountCooldownUntil = now.Add(-time.Second)
	p.ApplyResult(a, "glm-5.3", nil)
	if a.AccountCooldownReason != "" || a.AccountCooldownBy != "" {
		t.Fatalf("stale reason kept after recovery: %q/%q", a.AccountCooldownReason, a.AccountCooldownBy)
	}
}

// TestDeadModelNeedsTwoDistinctAccounts: one account's "no such model" is that
// account's problem; two distinct accounts agreeing means the model is gone on
// that channel and must stop costing upstream calls.
func TestDeadModelNeedsTwoDistinctAccounts(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)

	notFound := func() *uperr.Error {
		return &uperr.Error{Class: uperr.ClassNotFound, Message: "model not found"}
	}
	// One account only: not dead yet, the other may still have it.
	p.ApplyResult(a, "ghost-model", notFound())
	if dead, _, _ := p.deadModelLocked(model.ChannelWorkBuddy, "ghost-model", now); dead {
		t.Fatal("one account's report must not condemn the model")
	}
	// The SAME account again: still one distinct account.
	p.ApplyResult(a, "ghost-model", notFound())
	if dead, _, _ := p.deadModelLocked(model.ChannelWorkBuddy, "ghost-model", now); dead {
		t.Fatal("repeated reports from one account must not condemn the model")
	}
	// A second distinct account: now it is a channel-wide fact.
	p.ApplyResult(b, "ghost-model", notFound())
	dead, why, n := p.deadModelLocked(model.ChannelWorkBuddy, "ghost-model", now)
	if !dead || n < 2 {
		t.Fatalf("two distinct accounts must condemn the model (dead=%v n=%d why=%q)", dead, n, why)
	}

	// Acquire refuses without touching any account.
	if _, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "ghost-model", ""); err == nil {
		t.Fatal("expected an error for a dead model")
	} else if _, ok := err.(*ErrModelGone); !ok {
		t.Fatalf("error = %T %v, want *ErrModelGone", err, err)
	}

	// A successful call proves it is back.
	p.ForgetDeadModel(model.ChannelWorkBuddy, "ghost-model")
	if dead, _, _ := p.deadModelLocked(model.ChannelWorkBuddy, "ghost-model", now); dead {
		t.Fatal("forget must clear the verdict")
	}
}

// TestDeadModelExpires: the verdict must not be permanent — a re-listed model
// has to come back without a restart.
func TestDeadModelExpires(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	cur := now
	p := New(Params{Now: func() time.Time { return cur }}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)
	nf := func() *uperr.Error { return &uperr.Error{Class: uperr.ClassNotFound, Message: "gone"} }
	p.ApplyResult(a, "m", nf())
	p.ApplyResult(b, "m", nf())
	if dead, _, _ := p.deadModelLocked(model.ChannelWorkBuddy, "m", cur); !dead {
		t.Fatal("expected a verdict")
	}
	cur = now.Add(deadModelTTL + time.Minute)
	if dead, _, _ := p.deadModelLocked(model.ChannelWorkBuddy, "m", cur); dead {
		t.Fatal("verdict must expire so the model can come back")
	}
}

// TestAdmissionThreeStates: a short rest keeps bound conversations alive while
// refusing new ones; a long rest refuses everything.
func TestAdmissionThreeStates(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), StickyGrace: 2 * time.Minute}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	p.Upsert(a)

	if got := p.AdmissionOf(a, "m"); got != AdmitNew {
		t.Fatalf("fresh account = %v, want AdmitNew", got)
	}
	// Still cooling: nothing may use it.
	a.AccountCooldownUntil = now.Add(time.Minute)
	if got := p.AdmissionOf(a, "m"); got != AdmitNone {
		t.Fatalf("cooling account = %v, want AdmitNone", got)
	}
	// Just out of a rest: bound conversations only.
	a.AccountCooldownUntil = now.Add(-30 * time.Second)
	if got := p.AdmissionOf(a, "m"); got != AdmitSticky {
		t.Fatalf("just-rested account = %v, want AdmitSticky", got)
	}
	// Grace elapsed: full admission again.
	a.AccountCooldownUntil = now.Add(-5 * time.Minute)
	if got := p.AdmissionOf(a, "m"); got != AdmitNew {
		t.Fatalf("grace elapsed = %v, want AdmitNew", got)
	}
	// Repeated failures without a rest: sticky only.
	a.BreakerFails = 2
	if got := p.AdmissionOf(a, "m"); got != AdmitSticky {
		t.Fatalf("failing account = %v, want AdmitSticky", got)
	}
}
