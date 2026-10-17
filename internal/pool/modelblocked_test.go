// ticket 33: model-level lock semantics — distinguish "every account is
// model-cooled" from "the pool has no usable account".
package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func coolModel(p *Pool, id, modelName string, until time.Time) {
	p.Lock()
	defer p.Unlock()
	for _, a := range p.AccountsLocked("workbuddy") {
		if a.ID == id {
			if a.ModelCooldowns == nil {
				a.ModelCooldowns = map[string]model.ModelCooldown{}
			}
			a.ModelCooldowns[modelName] = model.ModelCooldown{Until: until}
		}
	}
}

// TestModelBlockedAllCooled 全池对某模型冷却 → blocked（带最早解锁与账号数）；
// 任一账号可服务 → 非阻塞；禁用/暂停号不计入分母。
func TestModelBlockedAllCooled(t *testing.T) {
	p := New(Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 3}, nil)
	a1 := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy}
	a2 := &model.Account{ID: "wb-2", Channel: model.ChannelWorkBuddy}
	a3 := &model.Account{ID: "wb-3", Channel: model.ChannelWorkBuddy}
	p.Upsert(a1)
	p.Upsert(a2)
	p.Upsert(a3)

	until := time.Now().Add(30 * time.Minute)
	coolModel(p, "wb-1", "glm-5.3", until)
	coolModel(p, "wb-2", "glm-5.3", until.Add(10*time.Minute))

	blocked, u, count := p.ModelBlocked("workbuddy", "glm-5.3")
	if blocked || count != 0 {
		t.Fatalf("a3 can still serve: blocked=%v count=%d", blocked, count)
	}

	coolModel(p, "wb-3", "glm-5.3", until.Add(20*time.Minute))
	blocked, u, count = p.ModelBlocked("workbuddy", "glm-5.3")
	if !blocked || count != 3 {
		t.Fatalf("all cooled: blocked=%v count=%d, want true/3", blocked, count)
	}
	if u.Sub(until).Abs() > time.Minute {
		t.Fatalf("earliest unlock = %v, want ≈%v", u, until)
	}

	// 其他模型不受影响；过期冷却不算。
	if blocked, _, _ = p.ModelBlocked("workbuddy", "deepseek-v4-pro"); blocked {
		t.Fatal("other models must not be blocked")
	}
	coolModel(p, "wb-1", "expired-model", time.Now().Add(-time.Minute))
	if blocked, _, _ = p.ModelBlocked("workbuddy", "expired-model"); blocked {
		t.Fatal("expired cooldowns must not count")
	}

	// 禁用/暂停号不计入分母：禁用 a3 后 wb-1+wb-2 全冷 → 仍 blocked=2。
	p.SetDisabled("wb-3", true)
	blocked, _, count = p.ModelBlocked("workbuddy", "glm-5.3")
	if !blocked || count != 2 {
		t.Fatalf("disabled excluded: blocked=%v count=%d, want true/2", blocked, count)
	}
	p.SetDisabled("wb-3", false)
	p.SetPaused("wb-2", true)
	blocked, _, count = p.ModelBlocked("workbuddy", "glm-5.3")
	if !blocked || count != 2 {
		t.Fatalf("paused excluded: blocked=%v count=%d, want true/2", blocked, count)
	}

	// ModelLockRows：锁池清单按整池不可用优先；总数=可路由账号数。
	rows := p.ModelLockRows()
	if len(rows) == 0 {
		t.Fatal("want lock rows")
	}
	found := false
	for _, r := range rows {
		if r.Model == "glm-5.3" {
			found = true
			if r.Total != 2 || r.Locked != 2 {
				t.Fatalf("row = %+v, want locked 2/2 (a3 disabled, a2 paused)", r)
			}
			if r.FullyUnlockAt.IsZero() {
				t.Fatal("fully-unlock must be set")
			}
		}
	}
	if !found {
		t.Fatalf("glm-5.3 row missing: %+v", rows)
	}
}

// TestModelBlockedNoRoutableAccounts "池里真没号"不算模型阻塞。
func TestModelBlockedNoRoutableAccounts(t *testing.T) {
	p := New(Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 1}, nil)
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy})
	p.SetDisabled("wb-1", true)
	if blocked, _, count := p.ModelBlocked("workbuddy", "glm-5.3"); blocked || count != 0 {
		t.Fatalf("empty routable pool must not report model block: blocked=%v count=%d", blocked, count)
	}
}
