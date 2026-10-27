// Stream continuation (BP-107): keep a turn alive when the upstream drops the
// connection after the client has already seen part of the answer.
//
// # WHY
//
// The gateway has always refused to switch channels mid-stream, because a
// second binding would re-run the whole turn and the client would receive the
// same paragraphs twice. That rule is right, but it only considered the
// channel-switch remedy. The other remedy — carry on with the SAME account and
// channel — was missing, so any dropped socket after the first token ended the
// turn.
//
// Field evidence 2026-10-24: five WorkBuddy requests died mid-answer
// (`wsarecv: An existing connection was forcibly closed by the remote host`,
// 66s–122s in, on 180k-token conversations), plus ten cancellations from the
// first-byte window expiring. In the request log that is 17 of 10,107 requests
// (0.17%), but for an agent client one dropped socket is not a slow answer: it
// is a lost turn, with every tool call and file edit in it.
//
// # HOW
//
// The already-emitted text is appended to the caller's history as an assistant
// message, followed by an instruction to carry on, and the same driver/account
// is asked again. Its events are spliced into the same client stream, so the
// client sees one continuous answer with exactly one terminal event. This is
// the same technique the reference project (misakano) uses for output-limit
// truncation, applied here to transport drops.
//
// # WHEN NOT TO
//
//   - nothing reached the client: the ordinary cross-channel cascade is strictly
//     better than re-asking the same account, so the continuation is not used;
//   - tool arguments were truncated: half a JSON blob cannot be spliced, and
//     handing the client invalid JSON is worse than an honest error;
//   - the failure is not a transport one (content rejection, auth, rate limit):
//     the same account will say the same thing again;
//   - the budget is spent (continueMaxSegments), so a persistently dropping
//     upstream cannot make the gateway pay for segments forever.
package gateway

import (
	"strings"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// continueMaxSegments bounds how many extra upstream calls one client request
// may pay for. 2 means "initial + at most two continuations"; a persistently
// dropping upstream then surfaces its error instead of looping.
const continueMaxSegments = 2

// continueNudge is what the continuation request says. Kept short and literal:
// the model must resume from the cut point without repeating or apologising,
// and a long instruction would itself become content the model reacts to.
const continueNudge = "你上一条回复因连接中断被截断了。请从中断处继续输出剩余内容；不要重复已输出过的部分，不要道歉或解释，直接继续。"

// streamLedger accumulates what the client has already received, so a
// continuation can replay it and the final aggregate can describe the whole
// answer rather than only the last segment.
//
// Only text and thinking are recorded: a continuation can only be anchored on
// prose. Tool-call fragments set sawTool, which DISQUALIFIES the continuation
// (see the package comment).
type streamLedger struct {
	text     strings.Builder
	thinking strings.Builder
	sawTool  bool
	usage    ir.Usage
	usageSet bool
}

func (l *streamLedger) record(ev ir.StreamEvent) {
	switch ev.Kind {
	case ir.EvText:
		l.text.WriteString(ev.Delta)
	case ir.EvThinking:
		l.thinking.WriteString(ev.Delta)
	case ir.EvToolStart, ir.EvToolArgs:
		l.sawTool = true
	case ir.EvUsage:
		if ev.Usage != nil {
			l.addUsage(*ev.Usage)
		}
	case ir.EvAttemptReset:
		// BP-123: the failed attempt's fragments are being discarded
		// everywhere (sentinel guard, protocol writers) — the ledger's tool
		// flag belongs to them. Without this, one attempt dying mid-tool-call
		// would permanently disqualify the continuation for a LATER attempt
		// that legitimately streamed prose and then dropped.
		l.sawTool = false
	case ir.EvToolRestart:
		// BP-139: the continuation explicitly REDOES the interrupted tool
		// call (fresh, complete arguments) instead of splicing half-JSON —
		// so the truncation no longer disqualifies the continuation. The
		// text/thinking anchors stay: they are what the redo builds on.
		l.sawTool = false
	}
}

// addUsage folds one segment's usage into the running total. Input tokens are
// summed too: each segment is a separate billed call, and the panel's cost
// columns must show what was actually paid.
func (l *streamLedger) addUsage(u ir.Usage) {
	l.usage.InputTokens += u.InputTokens
	l.usage.OutputTokens += u.OutputTokens
	l.usage.CachedInputTokens += u.CachedInputTokens
	l.usage.ReasoningTokens += u.ReasoningTokens
	l.usage.Credit += u.Credit
	l.usage.CacheKnown = l.usage.CacheKnown || u.CacheKnown
	l.usage.CreditKnown = l.usage.CreditKnown || u.CreditKnown
	l.usageSet = true
}

// canContinue decides whether this failure may be resumed on the same account.
// It is deliberately conservative: every false negative costs one dropped
// turn, every false positive risks duplicate or malformed output.
func (l *streamLedger) canContinue(class string, segs int) bool {
	if segs >= continueMaxSegments {
		return false
	}
	if l.text.Len() == 0 && l.thinking.Len() == 0 {
		return false // nothing on the wire to anchor on
	}
	// BP-139: a sawTool cut no longer disqualifies the continuation. The
	// partial arguments are DISCARDED (writers retire the buffered fragment,
	// the client never saw it on buffered protocols) and the continuation
	// request asks the model to REDO the call from scratch — so what the
	// client experiences is one continuous stream whose tool call arrives
	// complete. The old blanket refusal traded a recoverable drop for a lost
	// turn (field 2026-10-27 20:22:06: workbuddy EOF at 117s mid-call, whole
	// turn lost).
	switch class {
	case uperr.ClassTransport, uperr.ClassUpstreamTimeout:
		// Dropped socket (vendor edge closed it) and idle stall (no bytes for
		// the watchdog window) are the two user-visible forms of "the answer
		// stopped arriving". Both are properties of the connection, not of the
		// payload, so asking the same account again is meaningful.
		return true
	}
	return false
}

// continueToolNudge is the mid-call variant of the continuation instruction
// (BP-139): the previous segment was cut while the model was emitting a tool
// call. The partial arguments were discarded (the client never saw them), so
// the model must REDO the call from scratch — continuing a half-JSON blob is
// impossible and useless.
const continueToolNudge = "你上一条回复在调用工具时因连接中断被截断，那次工具调用的参数不完整、已被丢弃。请重新发起你当时要调用的工具调用（完整、从头生成参数）；之前已输出的正文内容保持有效，不要重复，不要道歉或解释。"

// continuationRequest builds the request for one more segment: the caller's
// history, what the client already saw as an assistant turn, then the nudge.
// When the cut happened mid-tool-call (sawTool), the nudge asks the model to
// redo the call instead of continuing prose (BP-139).
//
// The caller's request is CLONED — the gateway is on a shared code path and a
// mutated request would leak the synthetic assistant turn into the next binding
// and into the caller's own retries.
func continuationRequest(req *ir.ChatRequest, ledger *streamLedger, redoTool bool) *ir.ChatRequest {
	clone := *req
	msgs := make([]ir.Message, 0, len(req.Messages)+2)
	msgs = append(msgs, req.Messages...)
	if txt := ledger.text.String(); txt != "" {
		msgs = append(msgs, ir.Message{
			Role:    ir.RoleAssistant,
			Content: []ir.ContentPart{{Kind: "text", Text: txt}},
		})
	}
	nudge := continueNudge
	if redoTool {
		nudge = continueToolNudge
	}
	msgs = append(msgs, ir.Message{
		Role:    ir.RoleUser,
		Content: []ir.ContentPart{{Kind: "text", Text: nudge}},
	})
	clone.Messages = msgs
	return &clone
}

// mergedResponse builds the aggregate the caller sees after several segments:
// the whole text the client received, the last segment's stop reason, and the
// summed usage (every segment was a separately billed call).
func mergedResponse(ledger *streamLedger, last *ir.ChatResponse) *ir.ChatResponse {
	out := &ir.ChatResponse{
		Message: ir.Message{
			Role:    ir.RoleAssistant,
			Content: []ir.ContentPart{{Kind: "text", Text: ledger.text.String()}},
		},
		Thinking: ledger.thinking.String(),
		Usage:    ledger.usage,
	}
	if last != nil {
		out.StopReason = last.StopReason
		out.Model = last.Model
		out.UpstreamID = last.UpstreamID
		out.CreatedAt = last.CreatedAt
		out.ThinkingSignature = last.ThinkingSignature
		out.Message.ToolCalls = last.Message.ToolCalls
	}
	if out.StopReason == "" {
		out.StopReason = ir.StopEnd
	}
	return out
}
