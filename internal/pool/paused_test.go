// ticket 32: paused exits routing but not keep-alive.
package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// TestPausedExitsRouting 暂停号退出选号（含冷却兜底路径），恢复后立即可选；
// 任务侧看到的账号列表不受影响（保号照常）。
func TestPausedExitsRouting(t *testing.T) {
	p := New(Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 1}, nil)
	acc := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy}
	p.Upsert(acc)

	if !p.SetPaused("wb-1", true) {
		t.Fatal("SetPaused on existing id must report true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, model.ChannelWorkBuddy, "glm-5.3", ""); err == nil {
		t.Fatal("paused account must not be selected")
	} else {
		var nc *ErrNoCapacity
		if !errors.As(err, &nc) {
			t.Fatalf("want ErrNoCapacity, got %v", err)
		}
	}
	// Keep-alive side: the account is still listed for task runners.
	if got := p.Accounts(model.ChannelWorkBuddy); len(got) != 1 || !got[0].Paused {
		t.Fatalf("Accounts = %+v, want the paused account listed with Paused=true", got)
	}

	if !p.SetPaused("wb-1", false) {
		t.Fatal("SetPaused(false) on existing id must report true")
	}
	lease, err := p.Acquire(ctx, model.ChannelWorkBuddy, "glm-5.3", "")
	if err != nil {
		t.Fatalf("resumed account must be selectable: %v", err)
	}
	lease.Release()

	if p.SetPaused("nope", true) {
		t.Fatal("SetPaused on unknown id must report false")
	}
}

// TestDisableClearsPaused 禁用是更强的终态：置 disabled 时清 paused。
func TestDisableClearsPaused(t *testing.T) {
	p := New(Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 1}, nil)
	acc := &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy}
	p.Upsert(acc)

	p.SetPaused("wb-1", true)
	p.SetDisabled("wb-1", true)
	if acc.Paused {
		t.Fatal("disable must clear paused (no conflicting labels)")
	}
	if !acc.Disabled {
		t.Fatal("account should be disabled")
	}
}
