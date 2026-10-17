package main

import (
	"testing"

	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
)

// The OpenCode Zen free lane has no login: the driver forges its own identity
// and never reads account credentials. But gateway.Execute filters out every
// binding whose channel has no account in the pool, so without a synthetic
// account the channel is unroutable ("no binding attempted") even though the
// driver alone would happily serve it. This pins that contract.
func TestEnsureAnonymousZenAccount(t *testing.T) {
	p := pool.New(pool.Params{}, map[string]int{}, func(*model.Account) {})
	var accounts []*model.Account

	accounts = ensureAnonymousZenAccount(p, accounts)
	accounts = ensureAnonymousZenAccount(p, accounts) // idempotent across restarts

	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1 (must not duplicate)", len(accounts))
	}
	if !p.HasAccounts(model.ChannelOpenCodeZen) {
		t.Fatal("pool reports no accounts for the opencodezen channel — the gateway would filter the channel out")
	}
	a := accounts[0]
	if a.Channel != model.ChannelOpenCodeZen {
		t.Fatalf("channel = %q, want %q", a.Channel, model.ChannelOpenCodeZen)
	}
	if len(a.CredsJSON) != 0 {
		t.Fatalf("synthetic account must stay credential-free, got %q", a.CredsJSON)
	}
	if a.Disabled || a.Paused {
		t.Fatal("synthetic account must be schedulable")
	}
}
