package pool

// BP-150: re-registering an account must not erase what the pool learned
// about it.
//
// The user's report: the AutoClaw row read "12673/12673" — the denominator is
// the high-water mark, and it had collapsed onto the current balance. The
// account once held more; after a re-import the panel could no longer show
// "自峰值已用 N", so a top-up arriving above the old peak would have been
// invisible (which is the whole point of BP-122).
//
// Cause: onboarding builds a FRESH *model.Account carrying only credentials and
// upserts it over the live one. Upsert replaced the whole object, so the peak,
// the cost tiers, the cooldowns and the failure counters went with it. Only
// AutoClaw has a re-import button (accounts/autoclaw/import-local), which is
// why no other pool showed the symptom.

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// TestUpsertKeepsLearnedPeak is the reported bug.
func TestUpsertKeepsLearnedPeak(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	p.Upsert(a)
	p.RecordCredits(a, model.Credits{Remain: 13000, Total: 13000, BalanceOnly: true, UpdatedAt: time.Unix(1000, 0)})
	p.RecordCredits(a, model.Credits{Remain: 12673, Total: 12673, BalanceOnly: true, UpdatedAt: time.Unix(2000, 0)})
	if a.Credits.Peak != 13000 {
		t.Fatalf("setup: peak = %d, want 13000", a.Credits.Peak)
	}

	// A re-import: fresh object, same id, credentials only.
	fresh := &model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw, Nickname: "acct",
		CredsJSON: []byte(`{"access_token":"new"}`)}
	p.Upsert(fresh)

	live := p.Accounts(model.ChannelAutoClaw)[0]
	if live.Credits.Peak != 13000 {
		t.Fatalf("re-import wiped the peak: peak = %d, want the historical 13000", live.Credits.Peak)
	}
	if live.Credits.PeakAt.Unix() != 1000 {
		t.Fatalf("peak_at = %v, want the arrival's timestamp", live.Credits.PeakAt)
	}
	if live.Credits.Remain != 12673 {
		t.Fatalf("remain = %d, want the last observed 12673", live.Credits.Remain)
	}
	// The new credentials must win — that is what a re-import is for.
	if string(live.CredsJSON) != `{"access_token":"new"}` {
		t.Fatalf("credentials were not replaced: %s", live.CredsJSON)
	}
}

// TestUpsertKeepsOtherLearnedState: the same hole existed for every observed
// field, so pin them together.
func TestUpsertKeepsOtherLearnedState(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	p.Upsert(a)
	p.RecordCredits(a, model.Credits{Remain: 500, BalanceOnly: true, UpdatedAt: time.Unix(1000, 0)})
	p.RecordCostTier(a, "glm-5.3", 2)
	p.mu.Lock()
	a.Windows = map[string]model.Window{"five_hour": {RemainingFraction: 0.4}}
	a.AccountCooldownUntil = time.Unix(9000, 0)
	a.AccountCooldownReason = "rate limit"
	a.AccountCooldownBy = "upstream"
	a.ConsecutiveFails = 3
	a.BreakerFails = 2
	a.LastUsedAt = time.Unix(8000, 0)
	p.mu.Unlock()

	fresh := &model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw, Nickname: "acct"}
	p.Upsert(fresh)

	live := p.Accounts(model.ChannelAutoClaw)[0]
	if live.Credits.Remain != 500 {
		t.Errorf("credits lost: remain = %d", live.Credits.Remain)
	}
	if _, ok := live.CostTiers["glm-5.3"]; !ok {
		t.Errorf("cost tiers lost")
	}
	if len(live.Windows) == 0 {
		t.Errorf("windows lost")
	}
	if live.AccountCooldownUntil.Unix() != 9000 || live.AccountCooldownReason != "rate limit" {
		t.Errorf("cooldown lost: %v %q", live.AccountCooldownUntil, live.AccountCooldownReason)
	}
	if live.ConsecutiveFails != 3 || live.BreakerFails != 2 {
		t.Errorf("failure counters lost: %d/%d", live.ConsecutiveFails, live.BreakerFails)
	}
	if live.LastUsedAt.Unix() != 8000 {
		t.Errorf("last_used_at lost")
	}
}

// TestUpsertDoesNotInventState: a brand-new account must stay empty — the
// merge must not fabricate observations for an id that never existed.
func TestUpsertDoesNotInventState(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := &model.Account{ID: "ac-new", Channel: model.ChannelAutoClaw, Nickname: "new"}
	p.Upsert(a)
	live := p.Accounts(model.ChannelAutoClaw)[0]
	if live.Credits.Peak != 0 || live.Credits.Remain != 0 {
		t.Fatalf("a new account invented credits: %+v", live.Credits)
	}
	if live.ConsecutiveFails != 0 {
		t.Fatalf("a new account invented failures")
	}
}

// TestUpsertHigherIncomingPeakWins: the peak is monotonic — whichever side
// observed more is the truth.
func TestUpsertHigherIncomingPeakWins(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	p.Upsert(a)
	p.RecordCredits(a, model.Credits{Remain: 100, BalanceOnly: true, UpdatedAt: time.Unix(1000, 0)})

	fresh := &model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw,
		Credits: model.Credits{Remain: 9000, BalanceOnly: true, Peak: 9000, PeakAt: time.Unix(5000, 0), UpdatedAt: time.Unix(5000, 0)}}
	p.Upsert(fresh)

	live := p.Accounts(model.ChannelAutoClaw)[0]
	if live.Credits.Peak != 9000 {
		t.Fatalf("peak = %d, want the higher 9000", live.Credits.Peak)
	}
}

// TestUpsertLowerIncomingPeakLoses is the other half: an incoming snapshot
// that carries its OWN peak but a LOWER one must not lower the historical
// mark. Both sides have a non-zero peak here, so this exercises the
// max-of-both rule rather than the "one side empty" fallback.
func TestUpsertLowerIncomingPeakLoses(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	p.Upsert(a)
	p.RecordCredits(a, model.Credits{Remain: 13000, BalanceOnly: true, UpdatedAt: time.Unix(1000, 0)})
	p.RecordCredits(a, model.Credits{Remain: 12673, BalanceOnly: true, UpdatedAt: time.Unix(2000, 0)})

	// A stale snapshot arrives from elsewhere, carrying a lower peak.
	stale := &model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw,
		Credits: model.Credits{Remain: 12673, BalanceOnly: true, Peak: 12673, PeakAt: time.Unix(3000, 0), UpdatedAt: time.Unix(3000, 0)}}
	p.Upsert(stale)

	live := p.Accounts(model.ChannelAutoClaw)[0]
	if live.Credits.Peak != 13000 {
		t.Fatalf("peak = %d, want the historical 13000 (a lower incoming peak must lose)", live.Credits.Peak)
	}
	if live.Credits.PeakAt.Unix() != 1000 {
		t.Fatalf("peak_at = %v, want the 13000 arrival's timestamp", live.Credits.PeakAt)
	}
}

// TestUpsertKeepsOperatorSwitchesWhenIncomingIsSilent is the boundary: the
// merge restores OBSERVATIONS, not the operator's decisions — a re-import that
// says nothing about Disabled/Paused must not silently re-enable an account
// someone turned off.
func TestUpsertKeepsOperatorSwitchesWhenIncomingIsSilent(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	a.Disabled = true
	a.Paused = true
	p.Upsert(a)

	// The incoming registration says nothing about the switches (both false,
	// as a fresh struct would).
	fresh := &model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw, Nickname: "acct"}
	p.Upsert(fresh)

	live := p.Accounts(model.ChannelAutoClaw)[0]
	if !live.Disabled || !live.Paused {
		t.Fatalf("a re-import silently re-enabled the account: disabled=%v paused=%v", live.Disabled, live.Paused)
	}
}
