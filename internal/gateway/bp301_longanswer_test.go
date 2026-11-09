package gateway

import (
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-301 (misakano 6e7fe62 clientLimitLongAnswer=32000)：客户端额度 ≥32000
// 也算「要长文」——上游实测 max_tokens=32000 在整 32000 处切半句、不带额度
// 自然写 60660（global:deepseek-v4.1-flash-sg「数到 20000」）。这一档是
// 客户端随手给的默认（ZCode 默认恰为 32000），不是「只要这么短」。
//
// 与信号 1/2 的差别：该分支不要求思考开启——关思考的客户端被纯长度 cap 切
// 一样该续。

func bp301Req(maxTokens int, thinking *ir.ThinkingConfig) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:     "m",
		Stream:    true,
		MaxTokens: maxTokens,
		Thinking:  thinking,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// 32000 预算、无档位（floor 未知）、正文占大头 → 信号 3 触发续写；续写段
// 保留客户端预算（32000 没被抬过）并关思考。
func TestBP301LongAnswerDefaultContinues(t *testing.T) {
	calls := 0
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: strings.Repeat("x", 5000)},
					{Kind: ir.EvDone, Stop: ir.StopLength}},
				&ir.ChatResponse{
					Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("x", 5000)}}},
					StopReason: ir.StopLength,
				}, nil
		},
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			calls++
			if call.Request.Thinking == nil || call.Request.Thinking.Enabled {
				t.Error("continuation must disable thinking")
			}
			if call.Request.MaxTokens != 32000 {
				t.Errorf("continuation max_tokens = %d, want the client's 32000 (nothing was raised)", call.Request.MaxTokens)
			}
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "-end"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	resp, res := runLC(t, drv, bp301Req(32000, &ir.ThinkingConfig{Enabled: true}))
	if calls != 1 {
		t.Fatalf("continuation calls = %d, want 1", calls)
	}
	if res.text.String() != strings.Repeat("x", 5000)+"-end" || resp.StopReason != ir.StopEnd {
		t.Fatalf("text tail=%q stop=%s", tailOf(res.text.String(), 10), resp.StopReason)
	}
	if res.dones != 1 {
		t.Fatalf("client saw %d terminals, want 1", res.dones)
	}
}

// 关思考的客户端 + 32000 → 一样续（信号 3 与思考无关，切它的是数字本身）。
func TestBP301LongAnswerContinuesWithThinkingOff(t *testing.T) {
	calls := 0
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		cutTextStep(),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			calls++
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "-end"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv, bp301Req(32000, &ir.ThinkingConfig{Enabled: false}))
	if calls != 1 {
		t.Fatalf("continuation calls = %d, want 1 (thinking-off client still continues)", calls)
	}
}

// <32000 的纯客户端限额（无档位、无思考、正文占大头）→ 不续。
func TestBP301ClientLimitBelowLongAnswerStaysTruncated(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		cutTextStep(),
	}}
	resp, res := runLC(t, drv, bp301Req(16384, &ir.ThinkingConfig{Enabled: false}))
	if len(drv.calls) != 1 {
		t.Fatalf("driver calls = %d, want 1 (pure client limit below %d)", len(drv.calls), clientLimitLongAnswer)
	}
	if resp.StopReason != ir.StopLength || res.lengths != 1 || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d lengths=%d", resp.StopReason, res.dones, res.lengths)
	}
}

// 信号 3 分支的 cap：≥2×clientLimitLongAnswer（65536）到顶即停——我方累计
// 口径下默认 32768 会让 32000 续写一次就死，该分支必须抬高。
func TestBP301LongAnswerCapStopsAt65536(t *testing.T) {
	seg := func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
		evs := []ir.StreamEvent{{Kind: ir.EvText, Delta: "s"}}
		evs = append(evs, ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{OutputTokens: 30000}})
		evs = append(evs, ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopLength})
		return evs, &ir.ChatResponse{
			Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "s"}}},
			StopReason: ir.StopLength,
			Usage:      ir.Usage{OutputTokens: 30000},
		}, nil
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){seg, seg, seg}}
	_, res := runLC(t, drv, bp301Req(32000, nil))
	// 30000 → 60000（续，<65536）→ 90000 ≥ 65536（停）：1 原生 + 2 续写。
	if len(drv.calls) != 3 {
		t.Fatalf("driver calls = %d, want 3 (long-answer cap 65536)", len(drv.calls))
	}
	if res.dones != 1 || res.lengths != 1 {
		t.Fatalf("dones=%d lengths=%d", res.dones, res.lengths)
	}
}

// cutTextStep 首段：5000 字符正文 + finish=length。
func cutTextStep() func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
	return func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
		return []ir.StreamEvent{{Kind: ir.EvText, Delta: strings.Repeat("x", 5000)},
				{Kind: ir.EvDone, Stop: ir.StopLength}},
			&ir.ChatResponse{
				Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("x", 5000)}}},
				StopReason: ir.StopLength,
			}, nil
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
