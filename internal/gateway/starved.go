// Starved-thinking rescue (BP-112): a turn that spent its ENTIRE output
// budget on reasoning and produced nothing else gets ONE retry with thinking
// off, at the same budget.
//
// # THE SHAPE
//
// Upstreams in the DeepSeek family bill reasoning tokens and answer tokens
// against the SAME max_tokens, with no separate thinking ceiling. When the
// client asks for a small budget, thinking can consume all of it: the turn
// ends finish_reason=length with zero answer text and no tool calls. The
// client sees an empty answer. Field-measured on the reference panel
// (misakano starved.go, 2026-10-22, cn:deepseek-v4.1-flash): budgets 60-300
// produced 0 answer bytes every time; disabling thinking at the same budget
// answered normally.
//
// # THE RESCUE
//
// Re-send ONCE, same account, same budget, thinking disabled and any effort
// gear removed. NOT a bigger budget: raising it would overspend what the
// client asked for, and a differently-sized answer is a new distinguishable
// shape. The retry reclaims the budget FROM thinking, which is the only thing
// that ate it.
//
// # WHEN NOT TO RESCUE
//
//   - the turn produced anything (text, thinking the client saw, tools): a
//     normal answer or a normal truncation is the client's own budget
//     semantics at work, and a second answer would be a surprise;
//   - no explicit max_tokens: without a client-declared budget there is no
//     "starved" to fix — the upstream default (32000 on most channels) is
//     orders of magnitude above what thinking can eat;
//   - the budget is below minStarvedBudget: liveness probes (max_tokens=1)
//     are better served failing fast than by doubling their cost;
//   - the client asked for thinking to be OFF already: the retry cannot
//     change anything.
package gateway

import (
	"bargainproxy/internal/ir"
)

// minStarvedBudget mirrors the reference: below this the retry costs more
// than the answer is worth (probes use max_tokens=1 and should just fail).
const minStarvedBudget = 16

// starvedRetryRequest rewrites req for the rescue call. ok=false when the
// turn does not match the starved shape or the retry cannot change anything.
func starvedRetryRequest(req *ir.ChatRequest, resp *ir.ChatResponse) (*ir.ChatRequest, bool) {
	if resp == nil || req == nil {
		return nil, false
	}
	if req.MaxTokens > 0 && req.MaxTokens < minStarvedBudget {
		return nil, false
	}
	// The shape: budget exhausted, NOTHING produced.
	if resp.StopReason != ir.StopLength {
		return nil, false
	}
	if len(resp.Message.ToolCalls) > 0 || ir.TextOf(resp.Message.Content) != "" {
		return nil, false
	}
	// Thinking the client already saw is content; a rescue would duplicate it
	// or contradict it. (Note: pure-reasoning turns that reached the client as
	// thinking already left evidence; the starved shape is the one where
	// nothing was surfaced at all.)
	if resp.Thinking != "" {
		return nil, false
	}
	// The client must have declared the budget that was eaten.
	if req.MaxTokens <= 0 {
		return nil, false
	}
	// Already off: nothing to change.
	if req.Thinking != nil && !req.Thinking.Enabled {
		return nil, false
	}
	clone := *req
	thinking := &ir.ThinkingConfig{Enabled: false}
	if req.Thinking != nil {
		// Keep Include semantics but disable the thinking itself: the point is
		// to free the budget, not to hide output the client wanted.
		thinking.Include = req.Thinking.Include
	}
	clone.Thinking = thinking
	// An effort gear asks FOR thinking; with thinking off it is contradictory
	// and the reference drops it (misakano: "关思考就别再带档位").
	clone.RequestedEffort = ""
	return &clone, true
}
