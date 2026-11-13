package gateway

import (
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-319：模型级冷却键别名统一。
//
// 冷却的键=e.Model（driver 填**上游名**），而 pick 循环查的键=Acquire 收到的
// **显示名**（客户端发的 canonical）。别名路由行（BP-218）下两个拼写不同 →
// 冷却静默失效：被冷却的模型经别名照样被选中、再撞同一个 429。
// 修法=网关在 ApplyResult 前把「点名本腿上游模型」的 e.Model 改写为显示名
// （rekeyCooldownModel）；点名**另一个**模型的错误（autoclaw 810000 报
// zai_auto，BP-116）保持原拼写——那道墙属于被点名的模型。

// TestBP319RekeyCooldownModel 纯函数分支覆盖。
func TestBP319RekeyCooldownModel(t *testing.T) {
	// 别名：上游名 ≠ 显示名 → 改写为显示名。
	e := uperr.New(uperr.ClassSoftRate, 429, "trae: model rate limit")
	e.Model = "kimi-k3.1-pro"
	rekeyCooldownModel(e, "kimi-k3.1-pro", "kimi-k3")
	if e.Model != "kimi-k3" {
		t.Fatalf("alias wall must be rekeyed to the display name, got %q", e.Model)
	}
	// 上游点名另一个模型（BP-116 autoclaw 810000 → zai_auto）：不动。
	e2 := uperr.New(uperr.ClassHardCredit, 402, "allowance used up")
	e2.Model = "zai_auto"
	rekeyCooldownModel(e2, "ali-glm-5.3", "glm-5.3")
	if e2.Model != "zai_auto" {
		t.Fatalf("a wall naming a DIFFERENT model must keep its own spelling, got %q", e2.Model)
	}
	// 两拼写相同：no-op。
	e3 := uperr.New(uperr.ClassSoftRate, 429, "wb: rate limit")
	e3.Model = "deepseek-v4.1-flash"
	rekeyCooldownModel(e3, "deepseek-v4.1-flash", "deepseek-v4.1-flash")
	if e3.Model != "deepseek-v4.1-flash" {
		t.Fatalf("same-spelling must be untouched, got %q", e3.Model)
	}
	// 显示名缺省（media/images 自带上游名的路径）：no-op。
	e4 := uperr.New(uperr.ClassSoftRate, 429, "wb: rate limit")
	e4.Model = "kimi-k3.1-pro"
	rekeyCooldownModel(e4, "kimi-k3.1-pro", "")
	if e4.Model != "kimi-k3.1-pro" {
		t.Fatalf("empty display must be a no-op, got %q", e4.Model)
	}
}

// TestBP319AliasWallStopsAliasRetry 端到端：上游名冷却必须拦住别名重试。
// 第一腿 429（driver 按惯例填上游名）→ 同渠道第二跳必须被冷却拦下（否则别名
// 下冷却不可见，同一个号被原样再打一遍）→ 级联到陪跑渠道。
func TestBP319AliasWallStopsAliasRetry(t *testing.T) {
	trae := &lcFake{channel: model.ChannelTrae, steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			// 真实 driver 的惯例：e.Model = call.UpstreamModel（上游名）。
			e := uperr.New(uperr.ClassSoftRate, 429, "trae: model rate limit")
			e.Model = call.UpstreamModel
			return nil, nil, e
		},
	}}
	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 4096,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	runLCC(t, trae, req, model.ChannelTrae, "kimi-k3.1-pro")
	if len(trae.calls) != 1 {
		t.Fatalf("alias-routed model wall must stop the second trae attempt: trae calls = %d, want 1 "+
			"(>1 means the wall written under the upstream spelling was invisible to the display-name pick)",
			len(trae.calls))
	}
}
