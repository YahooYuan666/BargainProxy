package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// BP-295：length 截断续写 + 终态持有。
//
// 上游把思考与正文记在同一份 max_tokens 里，思考把预算吃掉后回答断在半句
// （finish=length，正文非空）。此前网关对这种形态不做任何事：客户端拿到
// stop=length 的半句。misakano 2b9b773/34a3ae6/29db878/e54c6e0/562d0d8 的
// 修法：预算被网关抬过、或思考占大头 → 同号关思考续写，闸门=累计输出上限。
//
// 本文件同时钉住两个前提：
//  1. 续写/救援用的 context 必须还活着（BP-112 的救援自 10-25 起在 cancel()
//     之后的已取消 context 上发流，生产里恒 context canceled——假驱动不看
//     ctx 所以测试全绿；本文件所有 fake driver 都检查 ctx）；
//  2. 客户端恰好收到一个终态事件（写层 EvDone 即 [DONE]/message_stop，
//     续写段的事件必须拼在终态之前）。

type lcCall struct {
	req   *ir.ChatRequest
	ctxOK bool
}

type lcFake struct {
	mu    sync.Mutex
	calls []lcCall
	steps []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error)
}

func (d *lcFake) Channel() string                          { return model.ChannelWorkBuddy }
func (d *lcFake) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *lcFake) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	n := len(d.calls) + 1
	d.calls = append(d.calls, lcCall{req: call.Request, ctxOK: ctx.Err() == nil})
	d.mu.Unlock()
	if n > len(d.steps) || d.steps[n-1] == nil {
		return &ir.ChatResponse{StopReason: ir.StopEnd}, nil
	}
	step := d.steps[n-1]
	evs, resp, err := step(n, call)
	for _, ev := range evs {
		if err := emit(ev); err != nil {
			return nil, err
		}
	}
	return resp, err
}

type lcResult struct {
	text    strings.Builder
	dones   int
	lengths int
}

func runLC(t *testing.T, drv *lcFake, req *ir.ChatRequest) (*ir.ChatResponse, *lcResult) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
		// 尾位陪跑 binding：workbuddy 非末位时首字窗口才武装（末位 win=0），
		// 救援/续写「必须跑在 cancel() 之前」的回归靠它才打得到。
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{model.ChannelWorkBuddy: 1, model.ChannelQoder: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "acct-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	// qoder 池也必须有账号，否则该 binding 被剔除、workbuddy 退回末位（win=0）。
	p.Upsert(&model.Account{ID: "acct-q", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: drv, model.ChannelQoder: &lcFake{}}, st)

	res := &lcResult{}
	out, err := g.Execute(context.Background(), req, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvText:
			res.text.WriteString(ev.Delta)
		case ir.EvDone:
			res.dones++
			if ev.Stop == ir.StopLength {
				res.lengths++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out.Response, res
}

func lcReq(maxTokens int, gear string) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:           "m",
		Stream:          true,
		MaxTokens:       maxTokens,
		RequestedEffort: gear,
		Thinking:        &ir.ThinkingConfig{Enabled: true},
		Messages:        []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// 预算被网关抬过（2048 → high 档下限 8192）+ finish=length 半句 → 续写：
// 续写段必须关思考、带抬后的预算，且两次调用的 ctx 都活着、客户端只见一个终态。
func TestLengthContinuationOnRaisedBudget(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "partial"}, {Kind: ir.EvDone, Stop: ir.StopLength}},
				&ir.ChatResponse{
					Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "partial"}}},
					StopReason: ir.StopLength,
				}, nil
		},
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			if call.Request.Thinking == nil || call.Request.Thinking.Enabled {
				t.Errorf("continuation must disable thinking, got %+v", call.Request.Thinking)
			}
			if call.Request.RequestedEffort != "" {
				t.Errorf("continuation must drop the gear, got %q", call.Request.RequestedEffort)
			}
			if call.Request.MaxTokens != 8192 {
				t.Errorf("continuation max_tokens = %d, want 8192 (raised floor, e54c6e0)", call.Request.MaxTokens)
			}
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "-more"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	resp, res := runLC(t, drv, lcReq(2048, "high"))
	if res.text.String() != "partial-more" {
		t.Fatalf("client text = %q, want %q", res.text.String(), "partial-more")
	}
	if resp.StopReason != ir.StopEnd {
		t.Fatalf("final stop = %s, want end", resp.StopReason)
	}
	if res.dones != 1 {
		t.Fatalf("client saw %d terminal events, want exactly 1", res.dones)
	}
	if len(drv.calls) != 2 {
		t.Fatalf("driver calls = %d, want 2", len(drv.calls))
	}
	for i, c := range drv.calls {
		if !c.ctxOK {
			t.Errorf("call %d ran on a canceled context (BP-112 regression)", i+1)
		}
	}
}

// 思考占大头（字节比）+ finish=length → 续写，即使预算没被抬过（32000 > high 下限）。
func TestLengthContinuationOnThinkingDominant(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{
					{Kind: ir.EvThinking, Delta: strings.Repeat("t", 4000)},
					{Kind: ir.EvText, Delta: "short"},
					{Kind: ir.EvDone, Stop: ir.StopLength},
				},
				&ir.ChatResponse{
					Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "short"}}},
					Thinking:   strings.Repeat("t", 4000),
					StopReason: ir.StopLength,
				}, nil
		},
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			if call.Request.Thinking == nil || call.Request.Thinking.Enabled {
				t.Error("continuation must disable thinking")
			}
			if call.Request.MaxTokens != 32000 {
				t.Errorf("continuation max_tokens = %d, want the client budget 32000", call.Request.MaxTokens)
			}
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "-more"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	resp, res := runLC(t, drv, lcReq(32000, "high"))
	if res.text.String() != "short-more" {
		t.Fatalf("client text = %q", res.text.String())
	}
	if resp.StopReason != ir.StopEnd || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d", resp.StopReason, res.dones)
	}
}

// 纯客户端限额（预算够大、正文占大头、没有思考）→ length 是客户端自己的语义，
// 不续写，原样下发。
func TestPureClientLimitStaysTruncated(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: strings.Repeat("x", 5000)},
					{Kind: ir.EvDone, Stop: ir.StopLength}},
				&ir.ChatResponse{
					Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("x", 5000)}}},
					StopReason: ir.StopLength,
				}, nil
		},
	}}
	resp, res := runLC(t, drv, lcReq(32768, "high"))
	if len(drv.calls) != 1 {
		t.Fatalf("driver calls = %d, want 1 (no continuation)", len(drv.calls))
	}
	if resp.StopReason != ir.StopLength || res.lengths != 1 || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d lengths=%d", resp.StopReason, res.dones, res.lengths)
	}
}

// 累计输出上限（misakano 29db878）：floor 8192 × 4 = 32768，到顶即停并保持 length 终态。
func TestLengthContinuationStopsAtCumulativeCap(t *testing.T) {
	seg := func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
		evs := []ir.StreamEvent{{Kind: ir.EvText, Delta: "s"}}
		evs = append(evs, ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{OutputTokens: 12000}})
		evs = append(evs, ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopLength})
		return evs, &ir.ChatResponse{
			Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "s"}}},
			StopReason: ir.StopLength,
			Usage:      ir.Usage{OutputTokens: 12000},
		}, nil
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){seg, seg, seg, seg}}
	_, res := runLC(t, drv, lcReq(2048, "high"))
	// 12000 → 24000（续）→ 36000 ≥ 32768（停）：1 原生 + 2 续写。
	if len(drv.calls) != 3 {
		t.Fatalf("driver calls = %d, want 3 (stop at the cumulative cap)", len(drv.calls))
	}
	if res.text.String() != "sss" {
		t.Fatalf("client text = %q, want spliced segments", res.text.String())
	}
	if res.dones != 1 || res.lengths != 1 {
		t.Fatalf("dones=%d lengths=%d, want exactly one length terminal", res.dones, res.lengths)
	}
}

// length 截在工具调用里：v1 只续纯正文，工具形态不续（sawTool 拒绝）。
func TestLengthCutInToolCallDoesNotContinue(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{
					{Kind: ir.EvText, Delta: "some"},
					{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "c1", Name: "shell"}},
					{Kind: ir.EvDone, Stop: ir.StopLength},
				},
				&ir.ChatResponse{
					Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "some"}}},
					StopReason: ir.StopLength,
				}, nil
		},
	}}
	runLC(t, drv, lcReq(2048, "high"))
	if len(drv.calls) != 1 {
		t.Fatalf("driver calls = %d, want 1", len(drv.calls))
	}
}

// BP-112 回归：零正文 starved 救援必须在活 context 上跑（10-25 起它一直在
// 已取消的 context 上发流，生产恒败；本 fake 检查 ctx，旧代码必红）。
func TestStarvedRescueRunsOnLiveContext(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		// 只发终态：零正文、零可见思考（starved 形态）。
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvDone, Stop: ir.StopLength}},
				&ir.ChatResponse{StopReason: ir.StopLength}, nil
		},
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			if call.Request.Thinking == nil || call.Request.Thinking.Enabled {
				t.Error("rescue must disable thinking")
			}
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "rescued"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	resp, res := runLC(t, drv, lcReq(100, "high"))
	if res.text.String() != "rescued" || resp.StopReason != ir.StopEnd {
		t.Fatalf("text=%q stop=%s", res.text.String(), resp.StopReason)
	}
	if len(drv.calls) != 2 || !drv.calls[1].ctxOK {
		t.Fatalf("calls=%d ctxOK[1]=%v — rescue must run before the window cancel", len(drv.calls), drv.calls[1].ctxOK)
	}
	if res.dones != 1 {
		t.Fatalf("client saw %d terminal events, want 1", res.dones)
	}
}
