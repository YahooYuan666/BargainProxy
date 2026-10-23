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

	// 对照：连接被掐断（unexpected EOF / connection reset）同样不罚号。BP-101：
	// 2026-10-23 实测 AutoClaw CN 网关同一个 100k 请求 16 连跑全 200，但前 8 次
	// 里有 2 次 ~30s 后 unexpected EOF；罚号会产生 5s 冷却，冷却到期又落进 2 分钟
	// 的 sticky-only 带（StickyGrace），于是一次瞬时 EOF 让整个池 2 分钟拒新会话。
	// 所以 ClassTransport 现在是"零账号惩罚、可继续轮转重试"。
	p.ApplyResult(acc, "glm-5.3", uperr.New(uperr.ClassTransport, 0, "unexpected EOF"))
	if acc.ConsecutiveFails != 0 {
		t.Fatalf("transport: ConsecutiveFails = %d, want 0 (a dropped socket is not the account's fault)", acc.ConsecutiveFails)
	}
	if !acc.AccountCooldownUntil.IsZero() {
		t.Fatalf("transport: cooldown = %v, want zero", acc.AccountCooldownUntil)
	}
	// 但它必须仍然可轮转：同一次请求要能立刻重试这个健康账号。
	if !uperr.Rotatable(uperr.ClassTransport) {
		t.Fatal("transport must stay rotatable so the same account can be retried immediately")
	}
}
