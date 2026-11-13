// Stop-shaped rescue (BP-302 narration): a turn that ended finish=stop with
// PROSE the upstream considered complete, but that a client reads as "it
// stopped mid-thought" — the tail is a colon, comma, ellipsis, dash, unclosed
// code fence, or just no sentence-final punctuation, on a TOOL-CLASS request
// (tools declared AND tool turns in the history; roleplay clients end prose
// on colons as normal style and must not be touched). One continuation whose
// nudge names the lead-in ("you stopped on a preview sentence"); its text
// splices into the same stream. Misfire cost: one prefix-cache-hit upstream
// call; runs ONLY on workbuddy (same domain as the length continuation).
//
// BP-316: the tool-only half (BP-303, misakano 1037397) is REMOVED. It was
// built for Codex's "says one sentence and stops" (BP-293); Codex is no
// longer a served client (user, 2026-11-13: 「codex 我放弃了」), and its
// real-world harm is on record: ZCode's normal shape — a short confirmation
// right after every tool result — is exactly the tool-only trigger, so a
// model that had FINISHED got pushed into another tool call and the turn
// never ended (sess_1d74dbe9: 105 rounds of "blind and unable to stop",
// together with the BP-70 image cap BP-315 already fixed). Narration keeps
// running and gains the five field-tested gates misakano shipped after its
// own runaway (e8df35b6+92dadae3+baf9155d+79ba3d62, 2026-11-10/11):
//
//   - user-yield gate: a question tail or "please confirm" wording is a
//     DELIBERATE stop — continuing once got an agent's draft email sent to
//     the whole staff (misakano issue #209, two incidents);
//   - text-embedded tool-call markup, by completeness: a COMPLETE DSML/XML
//     block is executed by the client, so continuing would fabricate a
//     second call; a TRUNCATED block must be completed (25 of 76 narration
//     triggers in 40 minutes were such false positives on deepseek);
//   - structural closers (</summary>/</details>/</analysis>) and '&' count
//     as finished;
//   - 400B length gate on punctuation-type tails: an agent's "notes + list"
//     nearly always ends on a colon, so an unbounded rescue fired every
//     turn; structurally open tails (unclosed fence, truncated markup) are
//     exempt — those are genuinely unfinished at any length.
package gateway

import (
	"strings"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// narrationNudge (misakano c2003af) — must NAME the lead-in, or the model
// believes the task is already done and repeats its conclusion.
const narrationNudge = "你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，不要重复已经写过的内容，不要重新开头，不要道歉或解释。"

// narrationMaxBytes: a lead-in sentence measured 40~200B in the field;
// beyond 400B a "stopped on a colon" tail is almost certainly normal
// formatting (misakano 79ba3d62). Structurally open tails bypass this — see
// narrationTailStructurallyOpen.
const narrationMaxBytes = 400

// userYieldMarkers: prose that hands the turn back to the user. The gate
// must be independent of narrationTailSuspends — a question tail reads as
// sentence-final there, but it is the model ASKING, not stopping mid-thought.
var userYieldMarkers = []string{
	"请确认", "确认后", "等你确认", "待你确认", "等你回复",
	"是否需要", "需不需要", "要不要我", "要不要", "你希望", "请告知",
	"让我知道", "请你确认", "你确认后", "确认无误", "可以吗", "行吗", "好吗",
}

var userYieldMarkersEN = []string{
	"please confirm", "let me know", "should i ", "do you want", "would you like",
	"awaiting your", "waiting for your", "confirm before", "shall i ",
}

// toolCallMarkupMarkers: tool calls the model wrote as PLAIN TEXT. DSML is
// DeepSeek's own wire format leaking into prose (both full- and half-width
// pipe spellings seen in the field); the XML/antml shapes come from prompts
// that teach the call format.
var toolCallMarkupMarkers = []string{
	"｜｜DSML｜｜", "|DSML|",
	"</function_calls>", "<function_calls>",
	"antml:invoke", "<invoke name=",
	"</tool_calls>", "<tool_calls>",
}

// markupState grades text-embedded tool-call markup by completeness; the two
// states need OPPOSITE rescue decisions (misakano 92dadae3 — "DSML 修了又像
// 没修" came from treating them alike).
type markupState int

const (
	markupNone     markupState = iota // no tool-call markup at all
	markupComplete                    // block closed: the client will execute it
	markupPartial                     // block cut mid-way: prose ends on half a marker
)

// toolCallMarkupState grades the markup in text. A block is complete when
// the trimmed text ends on the closer the format ends with ("</function_calls>",
// "</tool_calls>", or DSML's "</｜｜DSML｜｜ calls>"); anything else that merely
// contains a marker is a truncated call.
func toolCallMarkupState(text string) markupState {
	if text == "" {
		return markupNone
	}
	found := false
	for _, m := range toolCallMarkupMarkers {
		if strings.Contains(text, m) {
			found = true
			break
		}
	}
	if !found {
		return markupNone
	}
	t := strings.TrimRight(text, " \t\r\n")
	switch {
	case strings.HasSuffix(t, "</function_calls>"), strings.HasSuffix(t, "</tool_calls>"):
		return markupComplete
	case strings.HasSuffix(t, "calls>") && strings.Contains(t, "DSML"):
		return markupComplete
	}
	return markupPartial
}

// textYieldsToUser reports whether the prose hands the turn back to the user:
// a question tail, or explicit "please confirm" wording (CN and EN).
func textYieldsToUser(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "？") || strings.HasSuffix(t, "?") {
		return true
	}
	for _, m := range userYieldMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	low := strings.ToLower(t)
	for _, m := range userYieldMarkersEN {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// narrationTailStructurallyOpen: the tail is structurally unfinished — a
// truncated tool-call marker or an unclosed code fence. Such tails are
// genuinely incomplete at ANY length, so the length gate does not apply.
func narrationTailStructurallyOpen(text string) bool {
	if toolCallMarkupState(text) == markupPartial {
		return true
	}
	return strings.Count(strings.TrimRight(text, " \t\r\n"), "```")%2 == 1
}

// narrationTailSuspends reports whether the prose ends mid-thought. Only
// structural markers count, never semantics.
func narrationTailSuspends(text string) bool {
	switch toolCallMarkupState(text) {
	case markupComplete:
		return false // client executes the block; continuing fabricates a second call
	case markupPartial:
		return true // client sees half a marker; the block must be completed
	}
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	for _, closer := range []string{"</summary>", "</details>", "</analysis>"} {
		if strings.HasSuffix(t, closer) {
			return false
		}
	}
	if strings.Count(t, "```")%2 == 1 {
		return true
	}
	rs := []rune(t)
	last := rs[len(rs)-1]
	if strings.ContainsRune("：:，,、—–…（(【[", last) {
		return true
	}
	// Sentence-final punctuation, closing brackets and '&' (RP clients end
	// turns on &&happy&&-style tags): a finished thought.
	if strings.ContainsRune("。.!！?？\"'”’））》】]}`*~&", last) {
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

// stopRescuePlan decides whether a finished stop-turn earns ONE narration
// rescue segment. tried is the once-per-request flag. Every gate is
// fail-closed: when in doubt, the original answer stands (a misfire costs
// one upstream call; a wrong "continue" can execute tools the model never
// meant to run).
func stopRescuePlan(channel string, req *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse, tried bool) (contReq *ir.ChatRequest, why string, ok bool) {
	if channel != model.ChannelWorkBuddy || req == nil || resp == nil || ledger == nil || tried {
		return nil, "", false
	}
	if resp.StopReason != ir.StopEnd {
		return nil, "", false
	}
	if ledger.sawTool || len(resp.Message.ToolCalls) > 0 {
		// A tool turn ended normally — nothing to rescue.
		return nil, "", false
	}
	text := ledger.text.String()
	if text == "" {
		return nil, "", false
	}
	// Same cumulative spending gate as the length path (floor × 4, default
	// 32768): a rescue may not push the turn past it.
	capTokens := defaultContinueTotalCap
	if f := effortFloor(req); f > 0 {
		capTokens = f * continueTotalCapRatio
	}
	if ledger.usage.OutputTokens >= capTokens {
		return nil, "", false
	}
	declared, toolTurn := toolClassRequest(req)
	if !declared || !toolTurn {
		return nil, "", false
	}
	// The model is waiting for the user's decision — a deliberate stop.
	// Continuing would take the action for them (misakano #209).
	if textYieldsToUser(text) {
		return nil, "", false
	}
	// Punctuation-type tails longer than a real lead-in are normal
	// formatting, not a cut-off thought. Structurally open tails (unclosed
	// fence, truncated marker) skip this gate — see above.
	if !narrationTailStructurallyOpen(text) && len(text) > narrationMaxBytes {
		return nil, "", false
	}
	if !narrationTailSuspends(text) {
		return nil, "", false
	}
	return continuationRequestNudge(req, ledger, narrationNudge),
		"the answer stopped on a lead-in sentence", true
}
