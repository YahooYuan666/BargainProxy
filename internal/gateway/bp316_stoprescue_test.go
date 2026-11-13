package gateway

import (
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-302 + BP-316：stop 形态的 narration 救援。
//
// BP-302（misakano c2003af）：stop + 正文停在预告句（冒号/省略号/未闭合代码
// 块/无句末标点）+ 工具类会话 → 一次续写，nudge 点明「你停在预告上」。
// BP-316：拆掉 BP-303 tool-only（为 Codex 而加，Codex 已不再服务；其实弹伤
// 害=把答完的 ZCode 回合推进多余工具调用，sess_1d74dbe9），narration 补上游
// 五道闸（misakano e8df35b6+92dadae3+baf9155d+79ba3d62）。

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
			rescueNudge = ir.TextOf(msgs[len(msgs)-1].Content)
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

// TestBP302NarrationGuards 三条负例：非工具会话 / 正常句号收尾 / 工具轮收尾
// 都不救。
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

// TestBP316YieldsToUser 模型在等用户拍板（问句 / 「请确认后我再…」）：这是
// 故意的收尾，绝不能救——上游实弹两例，要确认的邮件被直接群发（issue #209）。
func TestBP316YieldsToUser(t *testing.T) {
	// 「请确认」句。短、句号收尾——除了等用户闸没有别的闸拦得住它。
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("邮件正文已经写好，收件人填的是全体成员。请确认后我再发送。", 0),
	}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 1 {
		t.Fatalf("waiting-for-user prose must NOT be rescued: calls = %d, want 1", len(drv.calls))
	}
	// 问号收尾（在尾部判定里是「说完了」，必须靠等用户闸单独拦住）。
	drv2 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("这两处要一起改吗？", 0),
	}}
	runLC(t, drv2, agentReq(true, 0))
	if len(drv2.calls) != 1 {
		t.Fatalf("question tail must NOT be rescued: calls = %d, want 1", len(drv2.calls))
	}
}

// TestBP316NarrationLengthGate 预告句实测 40~200B；超过 400B 还停在冒号上的
// 是 agent 的「说明+清单」正常排版，不救（上游实测：无长度闸时每个工具轮都
// 被多续一次）。未闭合代码块不受此限。
func TestBP316NarrationLengthGate(t *testing.T) {
	// >400B 停在冒号上 → 不救。
	long := strings.Repeat("这一轮我把改动逐条过了一遍，下面是按文件整理的要点清单：", 12) // ~1100B
	if len(long) <= narrationMaxBytes {
		t.Fatalf("sample too short: %d", len(long))
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){stopStep(long, 0)}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 1 {
		t.Fatalf("1.1KB prose on a colon must NOT be rescued: calls = %d, want 1", len(drv.calls))
	}
	// 未闭合代码块：结构上确实没写完，长度无关，仍要救。
	fenced := strings.Repeat("先列出结论摘要。", 60) + "\n```go\nfunc x() {"
	drv2 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep(fenced, 0),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "……剩余代码"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv2, agentReq(true, 0))
	if len(drv2.calls) != 2 {
		t.Fatalf("unclosed fence is structurally open: calls = %d, want 2", len(drv2.calls))
	}
}

// TestBP316MarkupStates 写成文本的工具调用按完整性分流（misakano 92dadae3，
// 「DSML 修了又像没修」的修复点）：完整块客户端会执行→不救；截断块客户端只见
// 半截标记→必须救。
func TestBP316MarkupStates(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool // narrationTailSuspends 的期望值
	}{
		{"完整 DSML 块", "正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>", false},
		{"完整 XML 块", "调用：<function_calls><invoke name=\"x\"></invoke></function_calls>", false},
		{"截断在 parameter", "执行 pwsh -c 即可。</｜｜DSML｜｜ parameter>", true},
		{"截断在 invoke", "…<｜｜DSML｜｜ invoke name=\"read_file\">", true},
		{"结构收尾 summary", "报告/）。在此之前不发任何新请求。 </summary>", false},
		{"RP 情绪标记收尾", "没生气就最好啦  &&happy&&", false},
		{"普通冒号收尾", "普通的回答，以冒号结尾：", true},
		{"含 < 但不是标记", "温度 < 5℃ 时注意。", false},
		{"含 | 但不是标记", "a | b 位运算说明。", false},
	}
	for _, c := range cases {
		if got := narrationTailSuspends(c.text); got != c.want {
			t.Errorf("%s: narrationTailSuspends=%v want %v (%q)", c.name, got, c.want, c.text)
		}
	}
	// 端到端：完整 DSML 块不救。
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>", 0),
	}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 1 {
		t.Fatalf("complete markup block must NOT be rescued: calls = %d, want 1", len(drv.calls))
	}
	// 端到端：截断的 DSML 块必须救（客户端不该只见半截标记）。
	drv2 := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("执行 pwsh -c 即可。</｜｜DSML｜｜ parameter>", 0),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: " 下行 400-600 Mbps 稳定。"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv2, agentReq(true, 0))
	if len(drv2.calls) != 2 {
		t.Fatalf("truncated markup block MUST be rescued: calls = %d, want 2", len(drv2.calls))
	}
}
