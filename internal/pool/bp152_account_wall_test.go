package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-152: a hard-credit wall whose driver set an explicit ResetAt owns both
// the scope (account-level) and the end. Qoder's 110/112 are account-level
// usage counters (qoder2api-hub #108); the BP-116 fill used to turn them into
// a 60-second model hold, after which the drained account was picked again
// and hit the same wall every minute.
func TestDriverResetAtWallRestsTheWholeAccount(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	p := New(Params{Now: func() time.Time { return now }},
		map[string]int{model.ChannelQoder: 2},
		func(acc *model.Account) {})
	acc := &model.Account{ID: "q-1", Channel: model.ChannelQoder}
	p.Upsert(acc)

	reset := now.Add(30 * time.Minute)
	ue := uperr.New(uperr.ClassHardCredit, 429, "qoder: usage limit reached (112)")
	ue.Code = "112"
	ue.ResetAt = reset

	p.ApplyResult(acc, "glm-5.3-flash", ue)

	if acc.AccountCooldownUntil.IsZero() {
		t.Fatal("a driver-timed quota wall must rest the whole account")
	}
	if diff := acc.AccountCooldownUntil.Sub(reset); diff < -time.Second || diff > time.Second {
		t.Fatalf("account rested until %s, want the driver's ResetAt %s", acc.AccountCooldownUntil, reset)
	}
	if len(acc.ModelCooldowns) != 0 {
		t.Fatalf("no model may be cooled for an account-level wall: %v", acc.ModelCooldowns)
	}
}

// Without a driver ResetAt the BP-116 model-scope behaviour is untouched.
func TestCodedWallWithoutResetAtStillModelScoped(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	p := New(Params{Now: func() time.Time { return now }},
		map[string]int{model.ChannelAutoClaw: 2},
		func(acc *model.Account) {})
	acc := &model.Account{ID: "ac-1", Channel: model.ChannelAutoClaw}
	p.Upsert(acc)

	ue := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: 810000 allowance used up")
	ue.Code = "810000"

	p.ApplyResult(acc, "zaicoding_glm-5.3", ue)

	if !acc.AccountCooldownUntil.IsZero() {
		t.Fatalf("account locked until %s — BP-116 scope must survive BP-152", acc.AccountCooldownUntil)
	}
	if _, ok := acc.ModelCooldowns["zaicoding_glm-5.3"]; !ok {
		t.Fatal("the request's model must still be cooled when the driver set no ResetAt")
	}
}
