package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestAccountRestrictedLeavesRouting pins the 2026-10-20 fix: when upstream
// reports the ACCOUNT as restricted (workbuddy 403 code=11140 "request
// illegal", displayMsg "账号存在异常行为，已被限制使用"), the account must be
// disabled so the pool stops offering it. Before this, the error classified as
// internal: no state change, sticky conversations stayed bound to the banned
// account, and every turn failed over to another channel — the pool looked
// broken while one account was still healthy.
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

	if !banned.Disabled {
		t.Fatal("a restricted account must be disabled (left routing)")
	}

	// Whatever the previous sticky binding was, the next request must land on
	// a usable account and never return the disabled one.
	for i := 0; i < 4; i++ {
		l, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4.1-flash", "sess-1")
		if err != nil {
			t.Fatalf("acquire after restriction: %v", err)
		}
		if l.Account.ID == "wb-banned" {
			t.Fatalf("disabled account was selected again (bound was %s)", bound)
		}
		l.Release()
	}
}

// The class must be rotatable: the request should retry another account of
// the same channel immediately (the account is at fault, not the payload).
func TestAccountRestrictedRotates(t *testing.T) {
	if !uperr.Rotatable(uperr.ClassAccountRestricted) {
		t.Fatal("account_restricted must rotate to another account")
	}
}
