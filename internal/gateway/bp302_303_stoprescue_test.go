package gateway

import (
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-302/303：stop 形态救援。
//
// BP-302 narration（misakano c2003af）：stop + 正文停在预告句（冒号/省略号/
// 未闭合代码块/无句末标点）+ 工具类会话 → 一次续写，nudge 点明「你停在预告上」。
// BP-303 tool-only（misakano 1037397）：stop + 短句 + 末条是工具结果 + 长
// prompt → 续写一次只采纳工具调用，无工具则整段回滚（客户端看到的与接线前
// 逐字节一致）。BP-293 Codex「说一句就停」的网关侧方案。

// agentReq 构造一条 agent 会话：tools 声明 + 历史含工具轮；lastTool 控制末条
// 是否工具结果；usageIn 控制首段 usage 的输入 token 读数。
func agentReq(lastTool bool, usageIn int) *ir.ChatRequest {
	req := &ir.ChatRequest{
		Model:     "m",
		Stream:    true,
		MaxTokens: 4096,
		Thinking:  &ir.ThinkingConfig{Enabled: true},
		Tools:     []ir.ToolDef{{Name: "shell", Parameters: map[string]any{"type": "object"}}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: txtPart("查一下再动手")},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "shell", Arguments: "{\"cmd\":\"ls\"}"}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: txtPart("file-a.txt")},
		},
	}
	if !lastTool {
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: txtPart("继续")})
	}
	return req
}

// stopStep 首段：正文 text、finish=stop；usageIn>0 时带 EvUsage 输入读数。
func stopStep(text string, usageIn int) func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
	return func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
		evs := []ir.StreamEvent{{Kind: ir.EvText, Delta: text}}
		if usageIn > 0 {
			evs = append(evs, ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: usageIn, OutputTokens: 50}})
		}
		evs = append(evs, ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
		return evs, &ir.ChatResponse{
			Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: text}}},
			StopReason: ir.StopEnd,
		}, nil
	}
}

// TestBP302NarrationRescueSplices 预告句收尾 + 工具类会话 → 续写段正文拼进
// 同一条流，客户端一个终态。
func TestBP302NarrationRescueSplices(t *testing.T) {
	req := agentReq(true, 0)
	var rescueNudge string
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("先看路由表：", 0),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			msgs := call.Request.Messages
			last := msgs[len(msgs)-1]
			rescueNudge, _ = ir.TextOf(last.Content), ""
			// 续写请求必须把已输出正文作为 assistant 前缀带回去。
			if len(msgs) < 2 || ir.TextOf(msgs[len(msgs)-2].Content) != "先看路由表：" {
				t.Errorf("rescue must replay the seen text as an assistant turn, got %v", msgs[len(msgs)-2])
			}
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "下一步重启网卡"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	resp, res := runLC(t, drv, req)
	if !strings.Contains(rescueNudge, "预告") {
		t.Fatalf("rescue nudge must name the lead-in, got %q", rescueNudge)
	}
	if res.text.String() != "先看路由表：下一步重启网卡" {
		t.Fatalf("client text = %q, want spliced rescue", res.text.String())
	}
	if resp.StopReason != ir.StopEnd || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d", resp.StopReason, res.dones)
	}
	if len(drv.calls) != 2 {
		t.Fatalf("driver calls = %d, want 2", len(drv.calls))
	}
}

// TestBP302NarrationGuards 四条负例：非工具会话 / 正常句号收尾 / 已救援过 /
// 工具轮收尾都不救。
func TestBP302NarrationGuards(t *testing.T) {
	// 非工具会话（角色扮演的冒号收尾是文风）。
	req := agentReq(true, 0)
	req.Tools = nil
	for i := range req.Messages {
		if req.Messages[i].Role == ir.RoleTool || req.Messages[i].ToolCallID != "" {
			req.Messages[i] = ir.Message{Role: ir.RoleUser, Content: txtPart("旧消息")}
		}
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){stopStep("从前有座山：", 0)}}
	runLC(t, drv, req)
	if len(drv.calls) != 1 {
		t.Fatalf("non-tool request: calls = %d, want 1 (colon ending is prose style)", len(drv.calls))
	}
	// 正常句号收尾。
	drv2 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){stopStep("查完了，结论如下。", 0)}}
	runLC(t, drv2, agentReq(true, 0))
	if len(drv2.calls) != 1 {
		t.Fatalf("finished answer: calls = %d, want 1", len(drv2.calls))
	}
	// 工具轮正常收尾（模型真的调了工具）。
	drv3 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			evs := []ir.StreamEvent{
				{Kind: ir.EvText, Delta: "跑一下："},
				{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "c9", Name: "shell"}},
				{Kind: ir.EvDone, Stop: ir.StopEnd},
			}
			return evs, &ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv3, agentReq(true, 0))
	if len(drv3.calls) != 1 {
		t.Fatalf("tool turn: calls = %d, want 1", len(drv3.calls))
	}
}

// TestBP303ToolOnlyAdoptsTools 短句 + 末条工具结果 + 长 prompt → 续写段出工具
// 调用即采纳；续写段的正文不得流向客户端。
func TestBP303ToolOnlyAdoptsTools(t *testing.T) {
	var sawRescueBody bool
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("这是高价值面，先看完整表单。", 25000),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			sawRescueBody = true
			return []ir.StreamEvent{
					{Kind: ir.EvText, Delta: "这段话必须被丢弃"},
					{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "c2", Name: "shell"}},
					{Kind: ir.EvDone, Stop: ir.StopEnd},
				},
				&ir.ChatResponse{
					Message: ir.Message{
						Role:      ir.RoleAssistant,
						Content:   []ir.ContentPart{{Kind: "text", Text: "这段话必须被丢弃"}},
						ToolCalls: []ir.ToolCall{{ID: "c2", Name: "shell", Arguments: "{}"}},
					},
					StopReason: ir.StopEnd,
				}, nil
		},
	}}
	resp, res := runLC(t, drv, agentReq(true, 25000))
	if !sawRescueBody {
		t.Fatal("rescue segment must run")
	}
	if strings.Contains(res.text.String(), "这段话必须被丢弃") {
		t.Fatalf("tool-only rescue prose leaked to the client: %q", res.text.String())
	}
	if resp.StopReason != ir.StopEnd || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d", resp.StopReason, res.dones)
	}
	if len(resp.Message.ToolCalls) == 0 {
		t.Fatalf("adopted rescue must carry the tool calls, got %+v", resp.Message)
	}
}

// TestBP303ToolOnlyRollsBack 续写段只有正文 → 整段回滚：客户端文本与终态和
// 接线前完全一致。
func TestBP303ToolOnlyRollsBack(t *testing.T) {
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("这是高价值面，先看完整表单。", 25000),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "继续解释一段废话"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{
					Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "继续解释一段废话"}}},
					StopReason: ir.StopEnd,
				}, nil
		},
	}}
	resp, res := runLC(t, drv, agentReq(true, 25000))
	if res.text.String() != "这是高价值面，先看完整表单。" {
		t.Fatalf("rollback failed, client text = %q", res.text.String())
	}
	if resp.StopReason != ir.StopEnd || res.dones != 1 {
		t.Fatalf("stop=%s dones=%d", resp.StopReason, res.dones)
	}
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatal("rolled-back response must not carry tool calls")
	}
}

// TestBP303ToolOnlyGuards 负例：正文超 240B / 末条不是工具结果 / prompt 读数
// 不足，都不救。
func TestBP303ToolOnlyGuards(t *testing.T) {
	long := stopStep(strings.Repeat("x", 300)+"。", 25000)
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){long}}
	runLC(t, drv, agentReq(true, 25000))
	if len(drv.calls) != 1 {
		t.Fatalf("prose over toolOnlyMaxBytes: calls = %d, want 1", len(drv.calls))
	}
	// 末条是 user（闲聊的最后一跳）。
	drv2 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("好的，没问题。", 25000),
	}}
	runLC(t, drv2, agentReq(false, 25000))
	if len(drv2.calls) != 1 {
		t.Fatalf("last message not a tool result: calls = %d, want 1", len(drv2.calls))
	}
	// prompt 读数不足（usage 未知或短会话）。
	drv3 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("好的，没问题。", 100),
	}}
	runLC(t, drv3, agentReq(true, 100))
	if len(drv3.calls) != 1 {
		t.Fatalf("prompt under toolOnlyMinPrompt: calls = %d, want 1", len(drv3.calls))
	}
}
