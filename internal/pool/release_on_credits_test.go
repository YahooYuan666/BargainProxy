package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// TRAE-8 red-first tests (BP-98): a daily check-in that refills the balance
// must also end a plan-entitlement cooldown — the reference design
// (agentpoints scheduler.ReenableIfCredits + pool.go:245) is "check in →
// re-read credits → credits > 0 releases the rest", and without it a
// 1005-cooled account waits out its full 12h even though the quota came
// back at midnight. Disabled accounts stay disabled: only cooldowns lift.

func TestReleaseOnCreditsLiftsCooldown(t *testing.T) {
	p := New(Params{}, map[string]int{}, nil)
	acc := &model.Account{ID: "trae:u1", Channel: model.ChannelTrae}
	p.Upsert(acc)
	p.restAccount(acc, time.Now().Add(12*time.Hour), "plan entitlement exhausted (code=1005)", "upstream", time.Now())

	p.ReleaseOnCredits(acc, 500)

	got := findAccount(p, acc.ID)
	if got == nil {
		t.Fatal("account missing")
	}
	if !got.AccountCooldownUntil.IsZero() && got.AccountCooldownUntil.After(time.Now()) {
		t.Fatalf("cooldown must be lifted, still cooling until %v (reason %q)", got.AccountCooldownUntil, got.AccountCooldownReason)
	}
}

func TestReleaseOnCreditsKeepsDisabled(t *testing.T) {
	p := New(Params{}, map[string]int{}, nil)
	acc := &model.Account{ID: "trae:dead", Channel: model.ChannelTrae, Disabled: true}
	p.Upsert(acc)
	p.restAccount(acc, time.Now().Add(12*time.Hour), "restricted", "upstream", time.Now())

	p.ReleaseOnCredits(acc, 500)

	got := findAccount(p, acc.ID)
	if got == nil || !got.Disabled {
		t.Fatal("a disabled account must stay disabled — only cooldowns lift")
	}
}

func TestReleaseOnCreditsZeroBalanceNoop(t *testing.T) {
	p := New(Params{}, map[string]int{}, nil)
	acc := &model.Account{ID: "trae:u2", Channel: model.ChannelTrae}
	p.Upsert(acc)
	p.restAccount(acc, time.Now().Add(12*time.Hour), "plan entitlement exhausted", "upstream", time.Now())

	p.ReleaseOnCredits(acc, 0)

	got := findAccount(p, acc.ID)
	if got == nil || !got.AccountCooldownUntil.After(time.Now()) {
		t.Fatal("zero balance must not lift the cooldown (nothing came back)")
	}
}

func TestReleaseOnCreditsNotCoolingNoop(t *testing.T) {
	p := New(Params{}, map[string]int{}, nil)
	acc := &model.Account{ID: "trae:u3", Channel: model.ChannelTrae}
	p.Upsert(acc)

	p.ReleaseOnCredits(acc, 500) // must not panic or mutate

	got := findAccount(p, acc.ID)
	if got == nil || got.AccountCooldownUntil.After(time.Now()) {
		t.Fatal("a non-cooling account must stay as it was")
	}
}

func findAccount(p *Pool, id string) *model.Account {
	for _, a := range p.Accounts(model.ChannelTrae) {
		if a.ID == id {
			return a
		}
	}
	return nil
}
