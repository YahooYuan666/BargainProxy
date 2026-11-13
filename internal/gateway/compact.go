// BP-307: compaction requests must not carry tool declarations.
//
// ZCode's background compaction runs as an ordinary model turn — same system
// prompt, same history — with one added instruction appended as the final user
// message telling the model to answer in plain text. The tool declarations,
// however, still ride along, so the upstream model sees ~88 callable tools
// while being asked to summarise. It summarises, then calls one (a real Bash
// call in the 2026-11-10 report) — and ZCode's compaction runner then throws
// the whole turn away:
//
//	EHo() in zcode.cjs: toolCallCount > 0 →
//	  "Tool use is not allowed during compaction"
//
// The 40k-char summary was complete and discarded with it, and compaction does
// not retry, so the context simply keeps growing.
//
// The fix belongs here rather than in the model: a compaction turn is a pure
// text task, so it should never see a callable tool. Strip the declarations
// and the model has nothing to call.
package gateway

import (
	"strings"

	"bargainproxy/internal/ir"
)

// Compaction detection fingerprints. Both come verbatim from the instruction
// ZCode appends to a compaction request; requiring BOTH keeps an ordinary chat
// that merely quotes one of them from being mistaken for a compaction turn.
const (
	compactionMarkTextOnly = "Respond with TEXT ONLY"
	compactionMarkNoTools  = "Do NOT call any tools"
)

// isCompactionRequest reports whether req is a client-side compaction turn.
//
// Only the LAST user message is examined: the compaction instruction is
// always the segment appended to the end of the history it asks the model to
// summarise, and an earlier occurrence means the conversation itself once
// discussed compaction (a real chat about this very feature would otherwise
// be mangled).
func isCompactionRequest(req *ir.ChatRequest) bool {
	last := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == ir.RoleUser {
			last = i
			break
		}
	}
	if last < 0 {
		return false
	}
	text := ir.TextOf(req.Messages[last].Content)
	if text == "" {
		return false
	}
	return strings.Contains(text, compactionMarkTextOnly) &&
		strings.Contains(text, compactionMarkNoTools)
}

// stripToolsForCompaction clears every field that would let the upstream model
// emit a tool call, reporting whether anything was removed.
//
// All four fields go together on purpose: the drivers write "tools" only when
// len(req.Tools) > 0, but they write "tool_choice" independently (see
// wbAnthropicToolChoice in the workbuddy driver and the chat lane's string
// form). Clearing Tools alone would leave an orphaned tool_choice on a request
// with no tool declarations.
//
// History messages are deliberately untouched: the tool_use / tool_result
// pairs already in the conversation ARE the material being summarised, and
// removing them would gut the summary.
func stripToolsForCompaction(req *ir.ChatRequest) bool {
	if len(req.Tools) == 0 && req.ToolChoice == nil && req.ToolChoiceName == "" && !req.DisableParallel {
		return false
	}
	req.Tools = nil
	req.ToolChoice = nil
	req.ToolChoiceName = ""
	req.DisableParallel = false
	return true
}
