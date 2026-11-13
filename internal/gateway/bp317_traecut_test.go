package gateway

import (
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-317：trae 渠道的「伪装成 stop 的按模型截断」续写。
//
// 上游实弹（traework2api-panel fe0b0cdb，2026-11-10）：trae SOLO 单段输出硬
// 上限按变体不同（deepseek-v4.1-flash 恰 32000、glm-5.3__max 恰 16000），到
// 顶硬切但 done 报 finish_reason:"stop"——截断被伪装成正常结束，不带
// max_tokens 的请求照样撞。我方 length 续写原本只接 workbuddy 且只认
// finish=length，这个形态完全接不住。触发=stop+纯文本+累计输出≥该模型实测
// cap；cap 表只收实测值，整千 plateau WARN 负责扩表。

// traeCutStep 构造一段 trae 输出：text 以句号收尾（句号收尾不进 narration
// 救援，避免和 stop 救援抢同一轮）、OutputTokens=out、finish=stop。
func traeCutStep(text string, out int) func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
	return func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
		return []ir.StreamEvent{
				{Kind: ir.EvText, Delta: text},
				{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 1000, OutputTokens: out}},
				{Kind: ir.EvDone, Stop: ir.StopEnd},
			}, &ir.ChatResponse{
				Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: text}}},
				StopReason: ir.StopEnd,
			}, nil
	}
}

// TestBP317TraeCutContinues 已知 cap（deepseek-v4.1-flash=32000）+ stop + 纯
// 文本 → 同号续写：续写请求关思考、预算抬到 cap、已输出正文作 assistant
// 前缀；客户端只看到一个终态。
func TestBP317TraeCutContinues(t *testing.T) {
	drv := &lcFake{channel: model.ChannelTrae, steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		traeCutStep(strings.Repeat("模型写到第 31999 个 token 的时候，", 200)+"。", 32000),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			req := call.Request
			if req.Thinking == nil || req.Thinking.Enabled {
				t.Errorf("continuation must disable thinking, got %+v", req.Thinking)
			}
			if req.RequestedEffort != "" {
				t.Errorf("continuation must drop the effort gear, got %q", req.RequestedEffort)
			}
			if req.MaxTokens < 32000 {
				t.Errorf("continuation budget %d must cover the model cap 32000", req.MaxTokens)
			}
			msgs := req.Messages
			if len(msgs) < 2 || !strings.HasSuffix(ir.TextOf(msgs[len(msgs)-2].Content), "。") {
				t.Errorf("continuation must replay the seen text as an assistant turn")
			}
			return traeCutStep("接上：这一段是续写补完的正文。", 5000)(n, call)
		},
	}}
	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 4096,
		Thinking: &ir.ThinkingConfig{Enabled: true},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "写一篇长文"}}}},
	}
	resp, res := runLCC(t, drv, req, model.ChannelTrae, "deepseek-v4.1-flash")
	if len(drv.calls) != 2 {
		t.Fatalf("driver calls = %d, want 2 (native + cap continuation)", len(drv.calls))
	}
	if !strings.Contains(res.text.String(), "续写补完的正文") {
		t.Fatalf("continuation text missing: %q", res.text.String())
	}
	if resp.StopReason != ir.StopEnd || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d", resp.StopReason, res.dones)
	}
}

// TestBP317TraeBelowCapNoTrigger 已知模型但输出没到 cap（自然收尾）→ 不续。
func TestBP317TraeBelowCapNoTrigger(t *testing.T) {
	drv := &lcFake{channel: model.ChannelTrae, steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		traeCutStep(strings.Repeat("正常收尾的一段话。", 30), 5000),
	}}
	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 4096,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	runLCC(t, drv, req, model.ChannelTrae, "deepseek-v4.1-flash")
	if len(drv.calls) != 1 {
		t.Fatalf("below-cap stop must NOT continue: calls = %d, want 1", len(drv.calls))
	}
}

// TestBP317TraeUnknownPlateauNoTrigger 表外模型（glm-5.3-flashx 无实测值）+
// 整千 plateau → 不续，只留 WARN 扩表（不猜阈值纪律）。
func TestBP317TraeUnknownPlateauNoTrigger(t *testing.T) {
	drv := &lcFake{channel: model.ChannelTrae, steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		traeCutStep(strings.Repeat("表外模型的一段输出。", 200), 12000),
	}}
	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 4096,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	runLCC(t, drv, req, model.ChannelTrae, "glm-5.3-flashx")
	if len(drv.calls) != 1 {
		t.Fatalf("unregistered model must NOT continue (no guessed caps): calls = %d, want 1", len(drv.calls))
	}
}

// TestBP317WorkbuddyStopUnchanged workbuddy 的 stop 收尾（即使输出撞整千）
// 仍不触发 length 续写——cap 表形态是 trae 专属，防止派发改动外溢。
func TestBP317WorkbuddyStopUnchanged(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		traeCutStep(strings.Repeat("workbuddy 正常答完的内容。", 100), 32000),
	}}
	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 4096,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	runLC(t, drv, req)
	if len(drv.calls) != 1 {
		t.Fatalf("workbuddy stop-shape must NOT continue: calls = %d, want 1", len(drv.calls))
	}
}
