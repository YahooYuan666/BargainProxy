package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestAccountRestrictedLeavesRouting pins the 2026-10-20 fix (state changed
// at all) plus the BP-205 reversal of its terminal form: when upstream
// reports the ACCOUNT as restricted (workbuddy 403 code=11140 "request
// illegal", displayMsg "账号存在异常行为，已被限制使用"), the account must
// leave routing for ~a week — field history shows the upstream lifts the
// restriction in about that time, so it now rests instead of being disabled
// outright. Before the original fix, the error classified as internal: no
// state change, sticky conversations stayed bound to the banned account, and
// every turn failed over to another channel — the pool looked broken while
// one account was still healthy.
func TestAccountRestrictedLeavesRouting(t *testing.T) {
	now := time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	banned := mkAccount("wb-banned", model.ChannelWorkBuddy)
	good := mkAccount("wb-good", model.ChannelWorkBuddy)
	p.Upsert(banned)
	p.Upsert(good)

	// Bind a session to the banned account first: a restricted account must
	// not keep its sticky binding.
	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4.1-flash", "sess-1")
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	bound := lease.Account.ID
	lease.Release()

	ue := uperr.New(uperr.ClassAccountRestricted, 403, "workbuddy: account restricted")
	p.ApplyResult(banned, "deepseek-v4.1-flash", ue)

	if banned.Disabled {
		t.Fatal("BP-205: a restriction is a ~week-long rest, not a disable")
	}
	if !now.Add(6 * 24 * time.Hour).Before(banned.AccountCooldownUntil) {
		t.Fatalf("the restriction rest must run about a week, until %v", banned.AccountCooldownUntil)
	}
	if banned.AccountCooldownReason != "restricted by upstream" {
		t.Fatalf("cooldown reason = %q, want the restriction wording", banned.AccountCooldownReason)
	}

	// Whatever the previous sticky binding was, the next request must land on
	// a usable account and never return the resting one.
	for i := 0; i < 4; i++ {
		l, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4.1-flash", "sess-1")
		if err != nil {
			t.Fatalf("acquire after restriction: %v", err)
		}
		if l.Account.ID == "wb-banned" {
			t.Fatalf("resting account was selected again (bound was %s)", bound)
		}
		l.Release()
	}
}

// BP-146 reversed this rule. It used to say the request should retry another
// account of the same channel immediately ("the account is at fault, not the
// payload"). Coding2API measured what that costs: one request walked three
// accounts of the pool, and the account touched last was banned three minutes
// later (f428e33). The ban is per account or per egress IP — every further
// attempt in the same pool is another risk record. The bind loop still
// cascades to a DIFFERENT channel, which is a different upstream.
func TestAccountRestrictedDoesNotRotateWithinChannel(t *testing.T) {
	if uperr.Rotatable(uperr.ClassAccountRestricted) {
		t.Fatal("account_restricted must NOT rotate within the same channel (BP-146)")
	}
}
