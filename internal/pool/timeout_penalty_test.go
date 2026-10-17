// ticket 27: timeout / cancellation never penalize the account.
package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestApplyResultNoPenaltyOnTimeoutAndCanceled 超时与取消都不是账号的问题：
// ApplyResult 对两类必须零动作（不连败、不加冷却）——这正是「超时止损」里
// 的「不罚号」（agentpoints 55d1e4d 口径）。
func TestApplyResultNoPenaltyOnTimeoutAndCanceled(t *testing.T) {
	p := New(Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 1}, nil)
	acc := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy}
	p.Upsert(acc)

	for _, class := range []string{uperr.ClassUpstreamTimeout, uperr.ClassCanceled} {
		p.ApplyResult(acc, "glm-5.3", uperr.New(class, 0, "stall/cancel"))
		if acc.ConsecutiveFails != 0 {
			t.Fatalf("class %s: ConsecutiveFails = %d, want 0", class, acc.ConsecutiveFails)
		}
		if !acc.AccountCooldownUntil.IsZero() {
			t.Fatalf("class %s: cooldown = %v, want zero", class, acc.AccountCooldownUntil)
		}
	}

	// 对照：普通传输错误仍然计罚（口径不能被放宽）。
	p.ApplyResult(acc, "glm-5.3", uperr.New(uperr.ClassTransport, 0, "jitter"))
	if acc.ConsecutiveFails != 1 {
		t.Fatalf("transport: ConsecutiveFails = %d, want 1", acc.ConsecutiveFails)
	}
	if acc.AccountCooldownUntil.IsZero() {
		t.Fatal("transport: cooldown should be set")
	}
}
