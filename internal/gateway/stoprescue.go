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
	"unicode/utf8"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// narrationNudge (misakano c2003af) — must NAME the lead-in, or the model
// believes the task is already done and repeats its conclusion.
//
// BP-331 (misakano 85179d43, issue #273 second comment): the instruction
// rides the conversation as a role=user message, and AGENT clients cannot
// tell it from real user input — upstream field: one was taken for user
// approval and ran tools unattended for 9 minutes, invisible in the client's
// history. Every nudge therefore opens with a self-identifying prefix, and
// closes with an explicit exit ("if you actually already finished, output
// NOTHING") so a false positive does not grow a self-defense paragraph into
// the transcript (misakano f8d83465: the model's own field quote「上一条回复
// 并未中断…」). The exit demands ZERO output, not "one word" — content deltas
// stream straight through, so even「已完成」would have landed in the client
// text (issue #273 phenomenon 3, byte-verified).
const narrationNudge = "【BargainProxy 网关自动续写·非用户消息】你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，不要重复已经写过的内容，不要重新开头，不要道歉或解释。如果你确实已经写完了，什么都不要输出，直接结束这一轮。"

// narrationPromiseNudge (BP-327, misakano 09189a5) — the tail is a PROMISE
// ("我将…"/"我会…") rather than a lead-in. The nudge above must not be reused
// there: it says "you stopped on a colon", which the model contradicts in its
// own answer ("上一条并未中断" — a real field quote), and that argument ends up
// in the client's transcript. A promise names an action, so the nudge asks for
// the action, not for "the rest of the text". Same prefix and exit as above.
const narrationPromiseNudge = "【BargainProxy 网关自动续写·非用户消息】你上一条回复最后停在「我将…」「我会…」这类承诺句上就结束了，承诺的动作没有执行。请直接执行它：需要查看文件、执行命令或调用工具就直接调用，不要解释为什么停下，不要重复已经写过的内容，不要重新开头。如果你确实已经写完了，什么都不要输出，直接结束这一轮。"

// narrationMaxBytes: a lead-in sentence measured 40~200B in the field;
// beyond 400B a "stopped on a colon" tail is almost certainly normal
// formatting (misakano 79ba3d62). Structurally open tails bypass this — see
// narrationTailStructurallyOpen.
//
// BP-327 (misakano bd59564, issue #245): this gate applies ONLY to
// punctuation-type tails. A tail that hangs on no punctuation at all
// ("现在我确认完了，我将开始修改 check.go") is a CUT-OFF SENTENCE, and its
// length says nothing about whether it was finished — those are the most
// common shape in long sessions. Gating them by total length rescued nothing
// upstream. See narrationTailPunctuated.
const narrationMaxBytes = 400

// narrationMaxRescues bounds one request's narration rescue segments. TWO, not
// one (BP-327, misakano 09189a5): a long answer commonly announces twice
// ("我先说明结论，然后我将开始修改…" then stops again), and a single rescue
// hands the second announcement straight back to the client (issue #245). Total
// spend is still bounded by the cumulative output cap.
const narrationMaxRescues = 2

// tailHangPunct is the punctuation set whose trailing position means "more
// text must follow" — the punctuation-type suspensions.
const tailHangPunct = "：:，,、—–…（(【["

// userYieldMarkers (strong): direct ask-to-user wording. BP-331 (misakano
// 89c84fb3, PR #261 — the #209 regression fix that supersedes BP-327's
// last-sentence window): a confirm request is usually followed by the DRAFT
// ITSELF ("请确认后我再发送。\n\n收件人：…\n主题：…"), so a tail-only window
// reads the draft as the final sentence, misses the ask, and the rescue
// fires — the exact shape that once sent a draft email to the whole staff.
// Strong wording matches ANYWHERE in the text.
var userYieldMarkers = []string{
	"请确认", "请你确认", "你确认后", "等你确认", "待你确认", "等你回复", "让我知道",
	"please confirm", "let me know",
}

// userYieldDeliberation (weak): wording that CAN ask the user but is just as
// common in technical narration (misakano field sample: a 4083-char answer
// stopped on 「我将开始修改 internal/verify/check.go」 and matched
// 「为“是否需要更换”提供量化支撑」 three thousand chars earlier — the shape
// BP-327's whole-text Contains blocked on). Judged by POSITION only, three
// positions (misakano 6cd32a7d): the final sentence; the trailing question
// when only punctuation-less draft lines follow it; directly followed by
// 我/你 (「确认后我再发送」).
var userYieldDeliberation = []string{
	"是否需要", "需不需要", "要不要", "确认后", "确认无误", "请告知", "你希望",
	"可以吗", "行吗", "好吗",
	"should i ", "shall i ", "do you want", "would you like",
	"awaiting your", "waiting for your", "confirm before",
}

// narrationPromiseWords: first-person next-action wording in the tail — the
// only unpunctuated long suspensions worth rescuing (BP-331, misakano
// bb8a9536 + 75653945: a roleplay client's "一两句 + ||| 分条 + <face:…> 收尾、
// 通篇不写句号" is neither a punctuation hover nor a promise, and every
// normal reply used to be continued — 87 times a day, each billed at
// 0.2→8.58 credits).
var narrationPromiseWords = []string{
	"我将", "我会", "我来", "我去", "我要", "我打算", "我准备", "我继续",
	"接下来我", "下一步我", "现在开始", "现在我", "让我来",
	"i will ", "i'll ", "i am going to ", "let me ",
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

// yieldTailBytes bounds the window used to judge "hands the turn back to the
// user" when the final sentence runs long.
const yieldTailBytes = 200

// yieldTail returns the window after the LAST sentence break.
//
// BP-327 (misakano 09189a5): handing the turn back is always a property of the
// final sentence — wording in the middle, or even right before the tail, can be
// technical narration. A real field sample ended with
// "…为是否需要更换提供量化支撑。\n\n---\n\n我将开始修改 check.go", and the
// whole-text Contains below refused to rescue it because of 「是否需要」 in the
// middle. Judging by the last sentence both keeps the real ask (「…请确认后我再
// 发送。」) and stops the false positive.
func yieldTail(s string) string {
	// Strip trailing sentence punctuation FIRST: a properly finished last
	// sentence ("…请确认后我再发送。") would otherwise split at its own period
	// and yield an empty tail — silently disabling the gate.
	s = strings.TrimRight(s, "。！？!?\n ")
	if i := strings.LastIndexAny(s, "。！？!?\n"); i >= 0 {
		_, n := utf8.DecodeRuneInString(s[i:])
		s = s[i+n:]
	}
	return lastRunes(strings.TrimSpace(s), yieldTailBytes)
}

// lastRunes keeps at most n bytes from the end of s, on a rune boundary.
func lastRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// textYieldsToUser reports whether the prose hands the turn back to the user.
//
// BP-331 (misakano 89c84fb3 + 6cd32a7d, superseding BP-327's pure
// last-sentence window): STRONG ask wording matches anywhere — the confirm
// request is usually followed by the draft itself (issue #209 regression);
// WEAK wording is position-gated (final sentence / trailing question /
// addressed to 我|你), because in technical narration it sits thousands of
// chars before the stop. A question tail still reads as an ask immediately.
// The gate must be independent of narrationTailSuspends — a question tail is
// sentence-final there, but it is the model ASKING, not stopping mid-thought.
func textYieldsToUser(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "？") || strings.HasSuffix(t, "?") {
		return true
	}
	low := strings.ToLower(t)
	for _, m := range userYieldMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	// Weak wording, position-gated — three positions.
	if containsAnyOf(strings.ToLower(yieldTail(t)), userYieldDeliberation) {
		return true
	}
	if q := trailingQuestion(t); q != "" && containsAnyOf(strings.ToLower(q), userYieldDeliberation) {
		return true
	}
	return deliberationAddressesUser(low)
}

// containsAnyOf reports whether s contains any of the markers.
func containsAnyOf(s string, subs []string) bool {
	for _, m := range subs {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// trailingQuestion returns the body's LAST question sentence when only
// punctuation-less content (draft lines) follows it — "要不要现在发送？\n
// 收件人：…\n主题：…". A full sentence (any 。！？!?) after the question means
// the model kept talking and the question is just part of the narration
// (misakano 6cd32a7d).
func trailingQuestion(t string) string {
	i := strings.LastIndexAny(t, "？?")
	if i < 0 {
		return ""
	}
	if strings.ContainsAny(t[i+1:], "。！？!?") {
		return ""
	}
	// The question runs from the last sentence break before it.
	start := strings.LastIndexAny(t[:i], "。！？!?\n")
	if start >= 0 {
		start++
	}
	return t[start : i+1]
}

// deliberationAddressesUser reports whether a weak marker directly addresses
// the user: immediately followed by 我 or 你 (「确认后我再发送」；the plural
// 「我们/你们」 does NOT count — misakano 6cd32a7d). Technical narration
// (「为“是否需要更换”提供量化支撑」) touches neither. Expects the lowercased
// text; CN markers carry no case, EN markers are already lowercase.
func deliberationAddressesUser(t string) bool {
	for _, m := range userYieldDeliberation {
		for rest := t; ; {
			i := strings.Index(rest, m)
			if i < 0 {
				break
			}
			rest = rest[i+len(m):]
			switch {
			case strings.HasPrefix(rest, "我们"), strings.HasPrefix(rest, "你们"):
				// a statement about the team, not an ask
			case strings.HasPrefix(rest, "我"), strings.HasPrefix(rest, "你"):
				return true
			}
		}
	}
	return false
}

// narrationAckedDone reports whether the text ends on the nudge's
// "already finished" exit — the model replied 「已完成」 or done. That is an
// explicit declaration: rescuing again would resend the same body for another
// full-context bill (misakano f8d83465/75653945). Judged by SUFFIX, not by
// last sentence: the rescue segment splices directly onto the previous text
// with no separator, so sentence-splitting would weld the two together.
func narrationAckedDone(text string) bool {
	tail := strings.TrimRight(text, " \t\r\n「」\"'*_。")
	if strings.HasSuffix(tail, "已完成") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(tail), "done")
}

// narrationTailPromises reports whether the tail's last sentence carries
// first-person next-action wording — the only unpunctuated long suspensions
// that are genuinely cut-off sentences (BP-331, misakano bb8a9536).
func narrationTailPromises(text string) bool {
	tail := strings.ToLower(yieldTail(text))
	return containsAnyOf(tail, narrationPromiseWords)
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
	if strings.ContainsRune(tailHangPunct, last) {
		return true
	}
	// Sentence-final punctuation, closing brackets and '&' (RP clients end
	// turns on &&happy&&-style tags). BP-331 adds '>' (misakano 75653945):
	// <face:…>/<br> tag closers mean "said and done" for clients whose prose
	// ends on an emoji tag instead of a period.
	if strings.ContainsRune("。.!！?？\"'”’））》】]}`*~&>", last) {
		return false
	}
	return true
}

// narrationTailPunctuated reports whether the tail hangs on a PUNCTUATION
// mark — the suspension shape that the length gate is meant to constrain.
//
// BP-327 (misakano bd59564, issue #245): "notes + list:" is normal formatting,
// and the longer the answer the more likely a colon ending is intentional, so
// those tails must stay length-bounded. The OPPOSITE shape — a sentence cut
// with no punctuation at all ("我现在确认完了，我将开始修改 check.go") — is a
// broken sentence whose length says nothing; those are exactly the common case
// in long sessions and must never be gated by size.
func narrationTailPunctuated(text string) bool {
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	rs := []rune(t)
	return strings.ContainsRune(tailHangPunct, rs[len(rs)-1])
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
// rescue segment. tries is how many rescues this request already spent
// (bounded by narrationMaxRescues). Every gate is fail-closed: when in doubt,
// the original answer stands (a misfire costs one upstream call; a wrong
// "continue" can execute tools the model never meant to run).
func stopRescuePlan(channel string, req *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse, tries int) (contReq *ir.ChatRequest, why string, ok bool) {
	if channel != model.ChannelWorkBuddy || req == nil || resp == nil || ledger == nil || tries >= narrationMaxRescues {
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
	// BP-331 (misakano f8d83465/75653945): the model used the nudge's exit —
	// 「已完成」/done is an explicit "already finished"; the chain stops.
	if narrationAckedDone(text) {
		return nil, "", false
	}
	// BP-327 + BP-331 (misakano 75653945): the length gate constrains
	// punctuation-type suspensions ("notes + list:" is normal formatting and
	// gets longer as the answer does) AND — new — long unpunctuated text
	// WITHOUT promise wording: a roleplay client's no-period style is normal
	// formatting, not a cut sentence (field: 87 rescues a day on one client).
	// A tail cut with no punctuation IS a broken sentence when the last
	// sentence announces a next action ("我将开始修改 check.go") — those are
	// the most common shape in long sessions. Structurally open tails
	// (unclosed fence, truncated marker) skip the gate entirely.
	punctuated := narrationTailPunctuated(text)
	if !narrationTailStructurallyOpen(text) && len(text) > narrationMaxBytes &&
		(punctuated || !narrationTailPromises(text)) {
		return nil, "", false
	}
	if !narrationTailSuspends(text) {
		return nil, "", false
	}
	// Promise tails ("我将…"/"我会…") get their own nudge: the lead-in nudge
	// claims they stopped on a colon, which they did not, and the model answers
	// by arguing the point into the client's transcript (BP-327, 09189a5).
	nudge, why := narrationNudge, "the answer stopped on a lead-in sentence"
	if !punctuated {
		nudge, why = narrationPromiseNudge, "the answer stopped on a promise sentence"
	}
	return continuationRequestNudge(req, ledger, nudge), why, true
}
