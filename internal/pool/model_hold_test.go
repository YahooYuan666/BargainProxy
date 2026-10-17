// BP-49: a model-scoped rate limit holds the model out of routing for at most
// ModelRateHoldCap — the upstream reset (ResetAt) is kept for the panel, but a
// request arriving after the cap re-probes upstream instead of waiting out a
// window that can state hours (workbuddy 6004 said 7h44m on 2026-10-16).
package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func modelRateErr(modelName string, resetAt time.Time) *uperr.Error {
	e := uperr.New(uperr.ClassModelRate, 200, "workbuddy: 您的使用量已超出频率限制")
	e.Model = modelName
	e.Code = "6004"
	e.ResetAt = resetAt
	return e
}

func TestModelRateHoldIsCappedForEarlyProbe(t *testing.T) {
	cur := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }, MaxQueueWait: time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	acc := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy}
	p.Upsert(acc)

	reset := cur.Add(8 * time.Hour)
	ue := modelRateErr("deepseek-v4.1-flash", reset)
	p.ApplyResult(acc, "deepseek-v4.1-flash", ue)

	mc, ok := acc.ModelCooldowns["deepseek-v4.1-flash"]
	if !ok {
		t.Fatal("hold not recorded")
	}
	if got := mc.Until.Sub(cur); got != 2*time.Hour {
		t.Fatalf("routing hold = %s, want the 2h cap", got)
	}
	if !mc.ResetAt.Equal(reset) {
		t.Fatalf("upstream reset = %v, want %v", mc.ResetAt, reset)
	}
	if mc.Reason != "6004 model rate limit" {
		t.Fatalf("reason = %q", mc.Reason)
	}
	if mc.Message == "" {
		t.Fatal("upstream message must be kept for the panel")
	}
	// The client-facing error carries the gateway's own retry moment.
	if ue.HoldUntil.Sub(cur) != 2*time.Hour {
		t.Fatalf("HoldUntil = %v, want the capped barrier", ue.HoldUntil)
	}

	// Blocked until the cap, admitted after it (the probe).
	blocked, until, count := p.ModelBlocked(model.ChannelWorkBuddy, "deepseek-v4.1-flash")
	if !blocked || count != 1 || until.Sub(cur) != 2*time.Hour {
		t.Fatalf("blocked=%v until=%v count=%d", blocked, until, count)
	}
	if _, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4.1-flash", ""); err == nil {
		t.Fatal("cooled model must not be acquirable before the cap")
	}
	cur = cur.Add(2*time.Hour + time.Second)
	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatalf("after the cap the pool must admit the probe: %v", err)
	}
	lease.Release()

	// A failed re-probe pushes the barrier out by another cap (never shortens
	// an active hold), while the upstream reset stays authoritative.
	p.ApplyResult(acc, "deepseek-v4.1-flash", modelRateErr("deepseek-v4.1-flash", reset))
	mc = acc.ModelCooldowns["deepseek-v4.1-flash"]
	if got := mc.Until.Sub(cur); got != 2*time.Hour {
		t.Fatalf("re-fold barrier = %s, want another 2h", got)
	}
	if !mc.ResetAt.Equal(reset) {
		t.Fatalf("reset changed: %v", mc.ResetAt)
	}
}

// A short upstream reset stays authoritative — the cap never extends it.
func TestModelRateHoldHonorsShortReset(t *testing.T) {
	cur := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	acc := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy}
	p.Upsert(acc)

	reset := cur.Add(10 * time.Minute)
	p.ApplyResult(acc, "glm-5.3", modelRateErr("glm-5.3", reset))
	mc := acc.ModelCooldowns["glm-5.3"]
	if !mc.Until.Equal(reset) {
		t.Fatalf("until = %v, want the 10min upstream reset", mc.Until)
	}
}
