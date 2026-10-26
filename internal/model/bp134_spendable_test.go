package model

import "testing"

// BP-134: Spendable is the number routing and the panel must both answer
// "can this account still pay?" from. Qoder's plan pool legitimately reads 0
// while the add-on pack holds credits — the client's own usage page shows
// 套餐内 300/300 剩余 0 AND 资源包 382/400, and the user's question was exactly
// why our panel kept saying 0.
func TestSpendable(t *testing.T) {
	// Plan exhausted, add-on pack alive: the field shape (2026-10-27).
	qoder := Credits{Remain: 0, Total: 300, Bonus: 382, BonusTotal: 400}
	if got := qoder.Spendable(); got != 382 {
		t.Fatalf("plan 0 + bonus 382: Spendable = %d, want 382", got)
	}
	// Plan alive, no add-on: WorkBuddy shape — unchanged from Remain.
	wb := Credits{Remain: 215, Total: 350}
	if got := wb.Spendable(); got != 215 {
		t.Fatalf("plan-only: Spendable = %d, want 215", got)
	}
	// Balance-only channels (AutoClaw points, Lobster credits): the balance
	// itself, never doubled through a bonus field.
	bal := Credits{Remain: 10400, Total: 10400, BalanceOnly: true}
	if got := bal.Spendable(); got != 10400 {
		t.Fatalf("balance-only: Spendable = %d, want 10400", got)
	}
	// Everything spent: zero, not negative-defensive fiction.
	spent := Credits{Remain: 0, Total: 300, Bonus: 0}
	if got := spent.Spendable(); got != 0 {
		t.Fatalf("all spent: Spendable = %d, want 0", got)
	}
}
