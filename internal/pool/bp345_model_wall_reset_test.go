package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-345 end to end: the AutoClaw driver reports the 810000 wall as a
// model-scoped hard-credit error whose ResetAt is the allowance's own day
// boundary (ResetHard). The pool must hold THAT model on THAT account until
// that moment — not for the 60s probe, and not the whole account.
//
// The model-scoped branch used to pass time.Time{} as the reset, so the
// driver's moment was dropped on the floor and setModelCooldownHard fell back
// to now+60s. Field 2026-11-25: one account re-hit the wall four times in
// fifteen minutes, each hit ending a conversation's turn.
func TestModelScopedWallHonoursDriverReset(t *testing.T) {
	now := time.Date(2026, 11, 25, 22, 30, 49, 0, Shanghai)
	p := New(Params{Now: func() time.Time { return now }},
		map[string]int{model.ChannelAutoClaw: 2}, nil)
	a := &model.Account{ID: "ac-1", Channel: model.ChannelAutoClaw}
	p.Upsert(a)

	reset := NextDay4AM(now)
	ue := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: GLM-5.4 今日用量额度已用完（810000）")
	ue.Code = "810000"
	ue.Model = "glm-5.4"
	ue.ResetAt = reset
	ue.ResetHard = true

	p.ApplyResult(a, "glm-5.4", ue)

	if !a.AccountCooldownUntil.IsZero() {
		t.Fatalf("the whole account was rested until %s; only the walled model may rest", a.AccountCooldownUntil)
	}
	mc, ok := a.ModelCooldowns["glm-5.4"]
	if !ok {
		t.Fatalf("no model cooldown recorded: %v", a.ModelCooldowns)
	}
	if !mc.Until.Equal(reset) {
		t.Fatalf("model held until %s, want the driver's day boundary %s (a 60s hold means the moment was dropped)", mc.Until, reset)
	}
	if !ue.HoldUntil.Equal(reset) {
		t.Fatalf("HoldUntil = %s; the client-facing error must carry the real wait", ue.HoldUntil)
	}

	// One minute later — past the old 60s probe — the walled model is still
	// refused while the account's other models keep serving.
	now = now.Add(65 * time.Second)
	if _, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.4", ""); err == nil {
		t.Fatal("the walled model was handed out again after 65s: that is the every-minute re-hit")
	}
	lease, err := p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.3-flash", "")
	if err != nil {
		t.Fatalf("the account's other models must keep serving during the wall: %v", err)
	}
	lease.Release()

	// After the day boundary the model is back.
	now = reset.Add(time.Second)
	lease, err = p.Acquire(context.Background(), model.ChannelAutoClaw, "glm-5.4", "")
	if err != nil {
		t.Fatalf("the wall outlived its own day boundary: %v", err)
	}
	lease.Release()
}

// A wall with no driver moment keeps the BP-314 short probe: the 60s fallback
// exists for a one-minute capacity wobble (Google's bare 429) and must not be
// turned into a long hold by this change.
func TestModelScopedWallWithoutResetKeepsShortProbe(t *testing.T) {
	now := time.Date(2026, 11, 25, 22, 30, 49, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return now }}, map[string]int{model.ChannelAutoClaw: 2}, nil)
	a := &model.Account{ID: "ac-1", Channel: model.ChannelAutoClaw}
	p.Upsert(a)

	ue := uperr.New(uperr.ClassModelRate, 429, "google: RESOURCE_EXHAUSTED")
	ue.Model = "gemini-3.8-flash-high"

	p.ApplyResult(a, "gemini-3.8-flash-high", ue)

	mc := a.ModelCooldowns["gemini-3.8-flash-high"]
	if got := mc.Until.Sub(now); got != time.Minute {
		t.Fatalf("hold = %s with no driver moment, want the 60s probe", got)
	}
}

// The reason and message must survive into the account state the panel shows:
// the operator reads "810000 model allowance used up" and the driver's own
// sentence there, not an empty explanation.
func TestModelScopedWallKeepsReasonAndMessage(t *testing.T) {
	now := time.Date(2026, 11, 25, 22, 30, 49, 0, Shanghai)
	p := New(Params{Now: func() time.Time { return now }}, map[string]int{model.ChannelAutoClaw: 2}, nil)
	a := &model.Account{ID: "ac-1", Channel: model.ChannelAutoClaw}
	p.Upsert(a)

	ue := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: GLM-5.4 今日用量额度已用完（810000）")
	ue.Code = "810000"
	ue.Model = "glm-5.4"
	ue.ResetAt = NextDay4AM(now)
	ue.ResetHard = true

	p.ApplyResult(a, "glm-5.4", ue)

	mc := a.ModelCooldowns["glm-5.4"]
	if mc.Reason != "810000 model allowance used up" {
		t.Fatalf("reason = %q", mc.Reason)
	}
	if mc.Message != ue.Message {
		t.Fatalf("message = %q, want the driver's sentence", mc.Message)
	}
	if !mc.ResetAt.Equal(ue.ResetAt) {
		t.Fatalf("ResetAt = %s, want %s recorded for the panel", mc.ResetAt, ue.ResetAt)
	}
}
