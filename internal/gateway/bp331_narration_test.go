package gateway

// BP-331: the rescue family follows misakano's third wave of field
// corrections (#261/#265 yield judging, #273 self-identifying nudges + ack
// exit + no-progress gate + chat-style gate).

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// STRONG ask wording matches ANYWHERE: the confirm request is usually
// followed by the draft itself, so the last-sentence-only window missed it
// (misakano #261, the #209 regression — a rescue here once mass-sent a draft
// email).
func TestBP331StrongYieldMarkerAnywhere(t *testing.T) {
	text := "草稿如下，请你确认。\n\n收件人：全员\n主题：周报\n\n以下是草稿正文，共三段。" +
		strings.Repeat("正文内容继续，", 40) + "我先把文件整理成目录结构再贴上来"
	if len(text) <= narrationMaxBytes {
		t.Fatalf("sample too short: %d", len(text))
	}
	if !textYieldsToUser(text) {
		t.Fatal("「请你确认」 in the middle (draft follows) must block the rescue")
	}
}

// WEAK wording mid-text is technical narration — the position gate must NOT
// block on it (the BP-327 field sample shape).
func TestBP331WeakMarkerMidTextDoesNotBlock(t *testing.T) {
	text := strings.Repeat("这些数据为是否需要更换提供量化支撑。", 30) + "\n\n我将开始修改 internal/verify/check.go"
	if textYieldsToUser(text) {
		t.Fatal("mid-text 「是否需要」 in technical narration must not block")
	}
	if narrationTailPunctuated(text) {
		t.Fatal("precondition: bare suspense")
	}
	if !narrationTailSuspends(text) {
		t.Fatal("precondition: the tail suspends")
	}
}

// Weak wording in the FINAL sentence still blocks.
func TestBP331WeakMarkerLastSentenceBlocks(t *testing.T) {
	if !textYieldsToUser("改动已全部落盘，是否需要我再跑一轮测试") {
		t.Fatal("final-sentence weak wording must block")
	}
}

// Weak wording right before draft lines — the trailing-question position.
func TestBP331TrailingQuestionBlocks(t *testing.T) {
	text := "方案说明若干。要不要现在发送？\n收件人：全员\n主题：周报"
	if !textYieldsToUser(text) {
		t.Fatal("a trailing question followed only by draft lines must block")
	}
	// A full sentence after the question = narration, not an ask.
	if textYieldsToUser("要不要现在发送？我认为先跑测试更稳。之后我将开始修改文件。") {
		t.Fatal("a question followed by full sentences is narration")
	}
}

// Weak wording directly followed by 我/你 addresses the user; the plural
// 我们/你们 does NOT count, and mid-text narration markers don't either.
func TestBP331DeliberationAddressesUser(t *testing.T) {
	if !textYieldsToUser("两个方案各有取舍，确认后我再发送草稿。") {
		t.Fatal("「确认后我再发送」 addresses the user")
	}
	if textYieldsToUser("等 staging 确认后我们再上线。\n\n日志格式确认无误后归档。\n\n我将开始修改部署脚本。") {
		t.Fatal("「我们」 and mid-text 「确认无误」 in narration must not block")
	}
}

// The nudge's exit reply (「已完成」/done) stops the chain.
func TestBP331AckDoneStopsRescuePlan(t *testing.T) {
	if !narrationAckedDone("已完成") || !narrationAckedDone("任务 completed. done") {
		t.Fatal("ack suffixes must be recognized")
	}
	if narrationAckedDone("全部测试已完成，报告在下面。") {
		t.Fatal("a properly finished sentence is not an ack")
	}
}

// The nudge texts carry the non-user-message prefix and the zero-output exit.
func TestBP331NudgeSelfIdentifiesAndOffersExit(t *testing.T) {
	for _, n := range []string{narrationNudge, narrationPromiseNudge} {
		if !strings.Contains(n, "非用户消息") {
			t.Fatalf("nudge must self-identify as gateway injection (misakano #273): %q", n)
		}
		if !strings.Contains(n, "什么都不要输出") {
			t.Fatalf("nudge must offer the zero-output exit: %q", n)
		}
	}
	// The promise nudge must still not claim a colon stop (BP-327 assertion).
	if strings.Contains(narrationPromiseNudge, "以冒号结尾") {
		t.Fatal("promise nudge must not claim a colon stop")
	}
}

// A long unpunctuated tail WITHOUT promise wording is normal formatting
// (roleplay/short-line clients) — not rescued (misakano #273: 87/day).
func TestBP331ChatStyleBareTailNotRescued(t *testing.T) {
	long := strings.Repeat("怎么啦妈 ||| 是干活累着了吗 ", 60) + "<face:抱抱>"
	if len(long) <= narrationMaxBytes {
		t.Fatalf("sample too short: %d", len(long))
	}
	if narrationTailPunctuated(long) {
		t.Fatal("precondition: no hang punctuation")
	}
	if narrationTailPromises(long) {
		t.Fatal("precondition: no promise wording")
	}
	// '>' now reads as a closer.
	if narrationTailSuspends(long) {
		t.Fatal("a tag-closed tail (> closer) is finished")
	}
}

// A long unpunctuated tail WITH promise wording is still rescued.
func TestBP331BarePromiseTailStillRescued(t *testing.T) {
	long := strings.Repeat("我把改动逐条过了一遍，以下是细节。", 30) + "现在我确认完了，我将开始修改 internal/gateway/stoprescue.go"
	if len(long) <= narrationMaxBytes {
		t.Fatalf("sample too short: %d", len(long))
	}
	if narrationTailPunctuated(long) || !narrationTailSuspends(long) {
		t.Fatal("precondition: bare suspense")
	}
	if !narrationTailPromises(long) {
		t.Fatal("precondition: promise wording in the tail")
	}
}

// The full gate: the plan refuses the chat-style tail and accepts the promise
// tail. ledger is built the way the rescue loop sees it after a stop turn.
func TestBP331StopRescuePlanGate(t *testing.T) {
	req := agentReq(true, 0)
	chatStyle := strings.Repeat("怎么啦妈 ||| 是干活累着了吗 ", 60) + "<face:抱抱>"
	if _, _, ok := stopRescuePlan("workbuddy", req, stopLedger(chatStyle), stopResp(chatStyle), 0); ok {
		t.Fatal("chat-style bare tail must not be rescued")
	}
	promise := strings.Repeat("我把改动逐条过了一遍，以下是细节。", 30) + "现在我确认完了，我将开始修改 internal/gateway/stoprescue.go"
	if _, _, ok := stopRescuePlan("workbuddy", req, stopLedger(promise), stopResp(promise), 0); !ok {
		t.Fatal("bare promise tail must be rescued")
	}
}

func stopLedger(text string) *streamLedger {
	l := &streamLedger{}
	l.text.WriteString(text)
	return l
}

func stopResp(text string) *ir.ChatResponse {
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: text}}},
		StopReason: ir.StopEnd,
	}
}
