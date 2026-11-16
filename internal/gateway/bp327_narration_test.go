package gateway

// BP-327：narration 救援三处实弹收窄（misakano bd59564 / 09189a5 / 89cdfe7，
// 2026-11-16 一天内第三次自我纠错）。三处必须一起改：
//
//  1. 长度闸门只该拦「标点型悬停」。标点型（「说明+清单：」）是排版习惯，越长
//     越可能是有意的；无标点悬停（「我将开始修改 check.go」）是句子被掐断，与
//     长度无关，长会话里最常见 —— 按整段长度一刀切，上游实测一条都不救。
//  2. 承诺句必须用另一条 nudge。复用「你停在冒号上」那条，模型会反过来跟网关
//     争「上一条并未中断」（真机原话），争论还写进客户端正文。
//  3. 救援上限 1 → 2。长正文模型常连写两段预告，只给一次 = 第二段又回到客户端
//     手里（issue #245 真实形态）。

import (
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// 长正文 + 无标点悬停必须救：这是 issue #245 的原始样本形态。
func TestBP327LongBareSuspenseStillRescued(t *testing.T) {
	long := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 40) // ≈1.6KB
	text := long + "现在我确认完了，我将开始修改 internal/gateway/stoprescue.go 里的闸门"
	if len(text) <= narrationMaxBytes {
		t.Fatalf("sample too short: %d", len(text))
	}
	if narrationTailPunctuated(text) || !narrationTailSuspends(text) {
		t.Fatalf("precondition: want a bare (non-punctuated) suspense, punctuated=%v suspends=%v",
			narrationTailPunctuated(text), narrationTailSuspends(text))
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep(text, 0),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "改完了。"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 2 {
		t.Fatalf("long prose on a bare suspense MUST be rescued (length says nothing here): calls = %d, want 2", len(drv.calls))
	}
}

// 长正文 + 标点型悬停仍不救：这是 issue #209 的成因，长度闸门就是为它存在的。
func TestBP327LongPunctuatedStillGated(t *testing.T) {
	long := strings.Repeat("这一轮我把改动逐条过了一遍，下面是按文件整理的要点清单：", 12)
	if len(long) <= narrationMaxBytes {
		t.Fatalf("sample too short: %d", len(long))
	}
	if !narrationTailPunctuated(long) {
		t.Fatalf("precondition: sample must end on a hang punctuation")
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){stopStep(long, 0)}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 1 {
		t.Fatalf("long prose on a colon is normal formatting: calls = %d, want 1", len(drv.calls))
	}
}

// 承诺句用承诺版 nudge，且不得复用「停在冒号上」那句。
func TestBP327PromiseTailUsesPromiseNudge(t *testing.T) {
	var nudge string
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("代码我看完了，问题在续写闸门。我将开始修改", 0),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			msgs := call.Request.Messages
			nudge = ir.TextOf(msgs[len(msgs)-1].Content)
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "改完了。"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 2 {
		t.Fatalf("promise tail must be rescued: calls = %d, want 2", len(drv.calls))
	}
	if !strings.Contains(nudge, "承诺句") {
		t.Fatalf("promise tail needs the promise nudge, got %q", nudge)
	}
	if strings.Contains(nudge, "以冒号结尾") {
		t.Fatalf("promise nudge must NOT claim the model stopped on a colon (the model argues back into the transcript): %q", nudge)
	}
}

// 冒号型悬停仍用原 nudge（不能被这次改动换掉）。
func TestBP327PunctuatedTailKeepsLeadInNudge(t *testing.T) {
	var nudge string
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		stopStep("先看路由表：", 0),
		func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
			msgs := call.Request.Messages
			nudge = ir.TextOf(msgs[len(msgs)-1].Content)
			return []ir.StreamEvent{{Kind: ir.EvText, Delta: "看完了。"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
				&ir.ChatResponse{StopReason: ir.StopEnd}, nil
		},
	}}
	runLC(t, drv, agentReq(true, 0))
	if !strings.Contains(nudge, "预告") {
		t.Fatalf("punctuation tail keeps the lead-in nudge, got %q", nudge)
	}
}

// 上限 2：连续两段都被判没写完 → 恰好再要 2 段，第 3 次不再救。
func TestBP327RescueCapIsTwo(t *testing.T) {
	unfinished := func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error) {
		// 每段都以未闭合代码块收尾 = 结构上确实没写完。
		return []ir.StreamEvent{{Kind: ir.EvText, Delta: "\n```go\nfunc x() {"}, {Kind: ir.EvDone, Stop: ir.StopEnd}},
			&ir.ChatResponse{StopReason: ir.StopEnd}, nil
	}
	drv := &lcFake{steps: []func(n int, call driver.ChatCall) ([]ir.StreamEvent, *ir.ChatResponse, error){
		unfinished, unfinished, unfinished, unfinished,
	}}
	runLC(t, drv, agentReq(true, 0))
	if len(drv.calls) != 1+narrationMaxRescues {
		t.Fatalf("calls = %d, want 1+%d (the cap must hold)", len(drv.calls), narrationMaxRescues)
	}
}

// 末句判据：中段出现「是否需要」不该拦（真实样本的形态），末句出现才算交回。
func TestBP327YieldJudgedByLastSentence(t *testing.T) {
	midTail := "……以上三点为是否需要更换交换机提供量化支撑。\n\n---\n\n我将开始修改 check.go"
	if textYieldsToUser(midTail) {
		t.Fatalf("wording in a middle sentence must not read as handing the turn back: %q", midTail)
	}
	finalAsk := strings.Repeat("先看了一轮日志。", 20) + "这版要现在发布吗？请确认后我再继续。"
	if !textYieldsToUser(finalAsk) {
		t.Fatalf("a final-sentence ask MUST still stop the rescue")
	}
	// 末句以句号收尾的请确认句不能因为「先剥尾部标点再切」而漏判。
	if !textYieldsToUser("邮件已写好。请确认后我再发送。") {
		t.Fatal("a properly finished 'please confirm' sentence must still be caught")
	}
}
