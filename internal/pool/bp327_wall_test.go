package pool

// BP-327: 冷却墙必须是纯墙钟（magpie 8769a58 / plugins#1126）。
//
// time.Now().Add(d) 返回值带 monotonic 读数，而**系统睡眠期间 monotonic 不推进**。
// 于是「00:40 到期」的墙在合盖睡到 09:00 之后、再醒来时仍然成立——墙被睡眠时长
// 白白推后。上游实弹：标注 00:40 到期的额度墙在 09:57 仍生效。
//
// 我方更痛：网关是 Windows 桌面常驻服务（笔记本常年合盖），且有一条 **7 天**量级
// 的墙（11140 风控休息），每晚睡一次就多漂几小时。
//
// 判据：所有写入 AccountCooldownUntil / ModelCooldowns[].Until / channelBusyUntil
// 的时间戳都不许带 monotonic 读数。

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// hasMonotonic 报告 t 是否带单调读数（Go 内部用墙钟+单调两个 int64 表达；
// 经 time.Date 往返后只剩墙钟）。
func hasMonotonic(t time.Time) bool {
	// time.Time.String() 在带单调读数时会追加 " m=+<秒>"。
	return len(t.String()) > 0 && containsMono(t.String())
}

func containsMono(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == ' ' && s[i+1] == 'm' && s[i+2] == '=' {
			return true
		}
	}
	return false
}

// onWall 必须把单调读数剥掉，且不改变墙钟读数本身。
func TestBP327OnWallStripsMonotonic(t *testing.T) {
	now := time.Now()
	if !hasMonotonic(now) {
		t.Skip("this platform/time does not expose a monotonic reading; nothing to assert")
	}
	got := onWall(now)
	if hasMonotonic(got) {
		t.Fatalf("onWall kept the monotonic reading: %s", got)
	}
	if !got.Equal(now) {
		t.Fatalf("onWall changed the wall clock: got %s want %s", got, now)
	}
	// 零点值原样返回（零时间表示「没有墙」，不能被改成一个真实时刻）。
	if z := onWall(time.Time{}); !z.IsZero() {
		t.Fatalf("onWall(0) = %s, want the zero time", z)
	}
}

// 账号冷却写入后不得带单调读数（这条钉住 restAccount 的调用链）。
func TestBP327RestAccountWallClockOnly(t *testing.T) {
	p := New(Params{Now: time.Now}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-1", model.ChannelWorkBuddy)
	p.Upsert(a)
	until := time.Now().Add(7 * 24 * time.Hour)
	p.mu.Lock()
	p.restAccount(a, until, "restricted by upstream", "fixed", time.Now())
	p.mu.Unlock()
	if hasMonotonic(a.AccountCooldownUntil) {
		t.Fatalf("restAccount stored a monotonic deadline: %s", a.AccountCooldownUntil)
	}
	if !a.AccountCooldownUntil.Equal(until) {
		t.Fatalf("restAccount changed the deadline: got %s want %s", a.AccountCooldownUntil, until)
	}
}

// 模型级冷却同理。
func TestBP327ModelCooldownWallClockOnly(t *testing.T) {
	p := New(Params{Now: time.Now}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("wb-1", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.mu.Lock()
	p.setModelCooldown(a, "m", time.Now().Add(2*time.Hour), "model rate limit", "6004", time.Now())
	p.mu.Unlock()
	mc := a.ModelCooldowns["m"]
	if hasMonotonic(mc.Until) {
		t.Fatalf("setModelCooldown stored a monotonic deadline: %s", mc.Until)
	}
}

// 渠道级暂停同理。
func TestBP327ChannelBusyWallClockOnly(t *testing.T) {
	p := New(Params{Now: time.Now}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	until := time.Now().Add(30 * time.Minute)
	p.mu.Lock()
	p.markChannelBusyLocked("wb", "m", until)
	p.mu.Unlock()
	p.mu.Lock()
	got := p.busyUntil["wb|m"]
	p.mu.Unlock()
	if hasMonotonic(got) {
		t.Fatalf("markChannelBusyLocked stored a monotonic deadline: %s", got)
	}
}
