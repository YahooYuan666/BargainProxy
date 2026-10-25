package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func bp116Pool(t *testing.T) (*Pool, *model.Account) {
	t.Helper()
	p := New(Params{Now: func() time.Time { return time.Date(2026, 10, 25, 23, 0, 0, 0, time.Local) }},
		map[string]int{model.ChannelAutoClaw: 2},
		func(acc *model.Account) {})
	acc := &model.Account{ID: "ac-1", Channel: model.ChannelAutoClaw}
	p.Upsert(acc)
	return p, acc
}

// BP-116: a quota wall that carries a business code but names no model must
// rest THAT MODEL, not lock the whole account to 04:00. Field evidence
// 2026-10-25: an AutoClaw account sat at "0 points, cooling to 04:00" whose
// only stated reason was a per-model 810000 wall that had expired three days
// earlier — one route's exhausted free allowance had been upgraded into a
// whole-account death sentence, hiding a healthy account.
func TestCodedQuotaWallRestsOnlyTheModel(t *testing.T) {
	p, acc := bp116Pool(t)
	ue := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: 810000 allowance used up")
	ue.Code = "810000" // Model left empty on purpose: the old hole

	p.ApplyResult(acc, "zaicoding_glm-5.3", ue)

	if !acc.AccountCooldownUntil.IsZero() {
		t.Fatalf("account locked until %s — a coded quota wall must not rest the whole account",
			acc.AccountCooldownUntil)
	}
	mc, ok := acc.ModelCooldowns["zaicoding_glm-5.3"]
	if !ok {
		t.Fatal("the model that ran out must be cooled")
	}
	if mc.Until.IsZero() {
		t.Fatal("model cooldown has no deadline")
	}
}

// A plain balance exhaustion (402, no business code) still takes the account
// path: the shared balance is gone for every model at once.
func TestUncodedBalanceWallStillRestsTheAccount(t *testing.T) {
	p, acc := bp116Pool(t)
	ue := uperr.New(uperr.ClassHardCredit, 402, "insufficient credit")
	// Code left empty: this is a plain balance wall.

	p.ApplyResult(acc, "some-model", ue)

	if acc.AccountCooldownUntil.IsZero() {
		t.Fatal("an uncoded balance wall must still rest the account")
	}
}

// A wall that already names its model keeps working as before.
func TestExplicitModelWallUnchanged(t *testing.T) {
	p, acc := bp116Pool(t)
	ue := uperr.New(uperr.ClassHardCredit, 402, "allowance used up")
	ue.Code = "810000"
	ue.Model = "zai_auto"

	p.ApplyResult(acc, "other-model", ue)

	if !acc.AccountCooldownUntil.IsZero() {
		t.Fatal("named-model wall must not rest the account")
	}
	if _, ok := acc.ModelCooldowns["zai_auto"]; !ok {
		t.Fatal("the named model must be cooled, not the request's model")
	}
	if _, ok := acc.ModelCooldowns["other-model"]; ok {
		t.Fatal("the request's model must be untouched when the error named another one")
	}
}
