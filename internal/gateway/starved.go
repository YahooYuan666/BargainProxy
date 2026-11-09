// Starved-thinking rescue (BP-112, extended BP-300): a turn whose output
// budget produced NOTHING the client can act on gets ONE retry with thinking
// off, at the same budget. Two shapes qualify:
//
//   - A (BP-112, misakano starved.go 2026-10-22): finish=length, zero answer
//     bytes, zero tool calls, NOTHING surfaced — thinking silently ate the
//     whole budget (cn:deepseek-v4.1-flash field: budgets 60-300 produced 0
//     answer bytes every time).
//   - B (BP-300, misakano 07f8ae0 2026-11-09 "emptiedByThinking"): finish=stop,
//     zero answer bytes, zero tool calls, but reasoning DID stream — the
//     upstream thinks the turn is complete ("thinking finished and it said
//     stop"), the client sees an empty reply (responses adapters answer it
//     with empty_reasoning_only / 502). The upstream's own line evidence is
//     codex/droid-class clients on 100k-token prompts.
//
// # THE RESCUE
//
// Re-send ONCE, same account, same budget, thinking disabled and any effort
// gear removed. NOT a bigger budget: raising it would overspend what the
// client asked for, and a differently-sized answer is a new distinguishable
// shape. The retry reclaims the budget FROM thinking, which is the only thing
// that ate it. For shape B the thinking already reached the client; the
// rescue segment has thinking OFF, so nothing is duplicated — the client just
// finally gets the answer.
//
// # WHEN NOT TO RESCUE
//
//   - the turn produced anything (text, tools): a normal answer or a normal
//     truncation is the client's own budget semantics at work, and a second
//     answer would be a surprise;
//   - no explicit max_tokens: without a client-declared budget there is no
//     "starved" to fix — the upstream default (32000 on most channels) is
//     orders of magnitude above what thinking can eat;
//   - the budget is below minStarvedBudget: liveness probes (max_tokens=1)
//     are better served failing fast than by doubling their cost;
//   - the client asked for thinking to be OFF already: the retry cannot
//     change anything;
//   - shape A but thinking already streamed to the client: what the client
//     saw is content; a rescue would add a second, disconnected thinking
//     stream (shape B is the surfaced-thinking case, and there the answer —
//     not more thinking — is what the rescue adds).
package gateway

import (
	"bargainproxy/internal/ir"
)

// minStarvedBudget mirrors the reference: below this the retry costs more
// than the answer is worth (probes use max_tokens=1 and should just fail).
const minStarvedBudget = 16

// starvedRetryRequest rewrites req for the rescue call. ok=false when the
// turn does not match either starved shape or the retry cannot change
// anything. The ledger is the authoritative record of what the CLIENT saw:
// driver-returned resp fields are not reliably filled on streamed turns, so
// the surfaced-thinking decision reads both.
func starvedRetryRequest(req *ir.ChatRequest, resp *ir.ChatResponse, ledger *streamLedger) (*ir.ChatRequest, bool) {
	if resp == nil || req == nil || ledger == nil {
		return nil, false
	}
	if req.MaxTokens > 0 && req.MaxTokens < minStarvedBudget {
		return nil, false
	}
	// Both shapes produced nothing the client can act on.
	if len(resp.Message.ToolCalls) > 0 || ledger.sawTool {
		return nil, false
	}
	if ir.TextOf(resp.Message.Content) != "" || ledger.text.Len() > 0 {
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
	// The two shapes part ways on stop reason and on what the client saw.
	surfaced := resp.Thinking != "" || ledger.thinking.Len() > 0
	switch {
	case resp.StopReason == ir.StopLength && !surfaced:
		// Shape A: thinking ate the budget in silence.
	case resp.StopReason == ir.StopEnd && surfaced:
		// Shape B: upstream thought, said "done", wrote no answer.
	default:
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
