// Stop-shaped rescues (BP-302 narration, BP-303 tool-only): turns that ended
// finish=stop with PROSE the upstream considered complete, but that a client
// reads as "it stopped mid-thought". No length, no transport error, no
// empty reply — every existing rescue is blind to these.
//
// Two shapes, one rescue slot each per request (misakano c2003af + 1037397):
//
//   - narration (c2003af): the answer stops on a LEAD-IN — tail is a colon,
//     comma, ellipsis, dash, unclosed code fence, or just no sentence-final
//     punctuation — on a TOOL-CLASS request (tools declared AND tool turns in
//     the history; roleplay clients end prose on colons as normal style and
//     must not be touched). One continuation whose nudge names the lead-in
//     ("you stopped on a preview sentence"), its text splices into the same
//     stream.
//   - tool-only (1037397): the answer is one glanceable sentence (≤240B) and
//     the LAST history message is a tool result — the model just read tool
//     output and should have acted. One continuation that asks it to ACT;
//     its text/thinking are DISCARDED (the client never sees them), and it is
//     adopted only if it produced tool calls — otherwise the original answer
//     and terminal stand, byte-for-byte what the client would have seen
//     unwired. This is the gateway-side remedy for the Codex "says one
//     sentence and stops" case (BP-293: the upstream honestly ended the turn;
//     misakano's field evidence: 38 of 156 same-shape samples end on a full
//     stop, so the adoption gate — tool calls only — keeps normal short
//     replies untouched).
//
// Both misfires cost one prefix-cache-hit upstream call; both run ONLY on
// workbuddy (same domain as the length continuation: this is that upstream's
// turn-ending behavior, and other channels bill thinking separately).
package gateway

import (
	"strings"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// narrationNudge (misakano c2003af) — must NAME the lead-in, or the model
// believes the task is already done and repeats its conclusion.
const narrationNudge = "你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，不要重复已经写过的内容，不要重新开头，不要道歉或解释。"

// toolOnlyNudge (misakano 1037397) — must say "stop explaining", or the model
// writes another summary whose text this rescue would discard, wasting the
// call.
const toolOnlyNudge = "继续执行下一步。需要查看文件、执行命令或使用工具时，直接调用工具，不要再用文字解释或重复已有结论。如果确实已经全部完成，请只回复 DONE。"

// toolOnlyMaxBytes: prose longer than this is not "one sentence" — normal
// short replies run <60 chars, the field's lead-in-then-stop samples 40~200.
const toolOnlyMaxBytes = 240

// toolOnlyMinPrompt: in a short session a one-line reply is normal (Q&A,
// greetings); only a LONG session makes it anomalous. Without a usage
// reading there is no way to tell — do not rescue.
const toolOnlyMinPrompt = 20000

// narrationTailSuspends reports whether the prose ends mid-thought. Only
// structural markers count, never semantics: colon/comma/ellipsis/dash/open
// bracket (something must follow), an unclosed ``` fence, and — as the
// catch-all — no sentence-final punctuation at all (English narration like
// "Reading the mutation script" ends bare).
func narrationTailSuspends(text string) bool {
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	if strings.Count(t, "```")%2 == 1 {
		return true
	}
	rs := []rune(t)
	last := rs[len(rs)-1]
	if strings.ContainsRune("：:，,、—–…（(【[", last) {
		return true
	}
	// Sentence-final punctuation and closing brackets: a finished thought.
	if strings.ContainsRune("。.!！?？\"'”’）)》】]}`*~", last) {
		return false
	}
	return true
}

// toolClassRequest reports whether the request is an AGENT turn: tools
// declared AND tool turns in the history. Either alone is not enough —
// roleplay clients declare dozens of tools and answer in plain prose, where
// a colon ending is just style (misakano: 20 of 40 field samples).
func toolClassRequest(req *ir.ChatRequest) (declared, toolTurn bool) {
	if req == nil {
		return false, false
	}
	declared = len(req.Tools) > 0
	for _, m := range req.Messages {
		if m.Role == ir.RoleTool || m.ToolCallID != "" || len(m.ToolCalls) > 0 {
			return declared, true
		}
	}
	return declared, false
}

// lastMessageIsToolResult: the strongest structural signal of an agent loop —
// each tool execution re-requests with the tool output last. Chat/roleplay
// turns end on user or assistant messages instead.
func lastMessageIsToolResult(req *ir.ChatRequest) bool {
	if req == nil || len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	return last.Role == ir.RoleTool || last.ToolCallID != ""
}

// stopRescuePlan decides whether a finished stop-turn earns ONE rescue
// segment, and of which kind. kind is "narration" or "toolonly". tried is the
// once-per-request flag; narration is checked first because a suspending tail
// gets the better remedy (its text splices; tool-only's text is discarded).
func stopRescuePlan(channel string, req *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse, tried bool) (contReq *ir.ChatRequest, kind, why string, ok bool) {
	if channel != model.ChannelWorkBuddy || req == nil || resp == nil || ledger == nil || tried {
		return nil, "", "", false
	}
	if resp.StopReason != ir.StopEnd {
		return nil, "", "", false
	}
	if ledger.sawTool || len(resp.Message.ToolCalls) > 0 {
		// A tool turn ended normally — nothing to rescue.
		return nil, "", "", false
	}
	text := ledger.text.String()
	if text == "" {
		return nil, "", "", false
	}
	// Same cumulative spending gate as the length path (floor × 4, default
	// 32768): a rescue may not push the turn past it.
	capTokens := defaultContinueTotalCap
	if f := effortFloor(req); f > 0 {
		capTokens = f * continueTotalCapRatio
	}
	if ledger.usage.OutputTokens >= capTokens {
		return nil, "", "", false
	}
	declared, toolTurn := toolClassRequest(req)
	if !declared || !toolTurn {
		return nil, "", "", false
	}
	if narrationTailSuspends(text) {
		return continuationRequestNudge(req, ledger, narrationNudge),
			"narration", "the answer stopped on a lead-in sentence", true
	}
	if len(text) > toolOnlyMaxBytes {
		return nil, "", "", false
	}
	if !lastMessageIsToolResult(req) {
		return nil, "", "", false
	}
	if ledger.usage.InputTokens < toolOnlyMinPrompt {
		// Unknown or short prompt: a one-line reply is normal there.
		return nil, "", "", false
	}
	return continuationRequestNudge(req, ledger, toolOnlyNudge),
		"toolonly", "one-line answer right after a tool result", true
}
