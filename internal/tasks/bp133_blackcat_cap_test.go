package tasks

import (
	"testing"

	"bargainproxy/internal/model"
)

// BP-133: the night-owl cap keeps the top-N accounts by credit balance —
// the same "夜猫子 Top-N" humanization misakano a24c10a shipped. Cap 0 (the
// default) and caps above the fleet size change nothing.
func TestCapBlackcatAccounts(t *testing.T) {
	mk := func(id string, remain int64) *model.Account {
		return &model.Account{ID: id, Credits: model.Credits{Remain: remain}}
	}
	accounts := []*model.Account{mk("a", 100), mk("b", 900), mk("c", 500), mk("d", 0)}
	got := capBlackcatAccounts(accounts, 2)
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("capped = %v, want [b c]", ids(got))
	}
	if all := capBlackcatAccounts(accounts, 0); len(all) != 4 {
		t.Fatalf("cap 0 must keep everyone, got %v", ids(all))
	}
	if all := capBlackcatAccounts(accounts, 10); len(all) != 4 {
		t.Fatalf("cap above fleet must keep everyone, got %v", ids(all))
	}
	// The input slice must not be reordered for the caller.
	if accounts[0].ID != "a" {
		t.Fatal("input order mutated")
	}
}

func ids(as []*model.Account) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}
