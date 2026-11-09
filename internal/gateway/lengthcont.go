// Length continuation (BP-295): keep a finish=length turn going when the
// budget the client named was never really the answer's budget.
//
// # WHY
//
// WorkBuddy settles thinking and answer tokens against the SAME max_tokens
// (misakano 2b9b773, production-measured 2026-11-08 on cn:deepseek-v4.1-flash:
// effort=high + max_tokens=2048 → total=4096, thinking eats 2048, the answer
// dies mid-sentence; effort=max thinking swings 7612~37110 on one prompt).
// The client sees stop=length with a half answer and no way to know why.
//
// Three signals say "this length was not the client's choice":
//  1. the gateway raised the budget to the effort floor on the way out — the
//     effective cap was OUR number, not the client's (34a3ae6 budgetWasRaised);
//  2. thinking dominates the answer — the same steal, measured after the fact
//     (562d0d8 starvedByThinking; this upstream reports no reasoning_tokens,
//     so bytes stand in for the token ratio);
//  3. the client's budget is ≥ clientLimitLongAnswer (6e7fe62): measured on
//     the reference panel (global:deepseek-v4.1-flash-sg, "count to 20000"),
//     max_tokens=32000 cut at exactly 32000 mid-sentence while the same prompt
//     without a limit ran to 60660 — this upstream gives-as-much-as-you-pay,
//     and a budget that large is a client DEFAULT (ZCode's own), not "I want
//     exactly this many tokens".
//
// Either one → continue on the SAME account with thinking disabled (the only
// thing that ate the budget), spliced into the same client stream by the
// terminal hold. Neither → the length is the client's own semantics and the
// answer goes out as it ended.
//
// # THE GATES (misakano 29db878)
//
// Segment count is only a backstop against "the continuation keeps
// truncating" loops (1 native + up to 7 continuations). The real spending
// gate is the CUMULATIVE output cap: floor × 4, or 32768 when no floor is
// known. Past it the answer stays truncated and the cap says so in the log —
// "burned our own cap" must not read like an upstream fault.
//
// Scope: workbuddy only, prose only. Other channels bill thinking separately
// (raising or continuing there would rewrite the client's own semantics);
// a length cut inside a tool call is the BP-139 redo shape and needs its own
// evidence before it earns a slot.
package gateway

import (
	"context"
	"fmt"
	"log"
	"strings"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
)

const (
	// lengthContMaxSegments bounds one client request's total upstream calls
	// for the length path (1 native + up to 7 continuations). The cumulative
	// cap below is the real gate; this only stops pathological loops.
	lengthContMaxSegments = 8
	// defaultContinueTotalCap applies when no floor is known (no gear).
	defaultContinueTotalCap = 32768
	// continueTotalCapRatio: cap = floor × ratio (misakano 29db878).
	continueTotalCapRatio = 4
	// continuationMinBudget mirrors config's probe floor: max_tokens below
	// this is a liveness probe, not a budget.
	continuationMinBudget = 16
	// clientLimitLongAnswer (misakano 6e7fe62): a client budget at or above
	// this is treated as "wants a long answer", not "wants exactly this many
	// tokens" — the field measurement behind it is in the package comment
	// (signal 3).
	clientLimitLongAnswer = 32000
)

// floorGear normalizes the client's requested gear for the floor lookup;
// ok=false when no thinking burns the shared budget (unset / off / none).
func floorGear(effort string) (string, bool) {
	gear := strings.ToLower(strings.TrimSpace(effort))
	switch gear {
	case "", "off", "none", "disabled":
		return "", false
	}
	return gear, true
}

// effortFloor is the output-budget floor for the request's effort gear, 0
// when no gear is set or the catalog has no floor for the model. Shared by
// the length continuation and the stop rescues (same cumulative cap).
func effortFloor(req *ir.ChatRequest) int {
	if req == nil {
		return 0
	}
	gear, ok := floorGear(req.RequestedEffort)
	if !ok {
		return 0
	}
	if f, known := config.MinOutputBudget(gear, req.Model, req.Model); known {
		return f
	}
	return 0
}

// lengthContinuationPlan decides whether a finish=length turn with a partial
// prose answer continues on the same account. It returns the rewritten
// request (thinking off, raised budget inherited) and a human reason for the
// log.
func lengthContinuationPlan(channel string, req *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse) (*ir.ChatRequest, string, bool) {
	if channel != model.ChannelWorkBuddy || req == nil || resp == nil {
		return nil, "", false
	}
	if resp.StopReason != ir.StopLength {
		return nil, "", false
	}
	if ledger.sawTool || ledger.text.Len() == 0 {
		// Zero text belongs to the BP-112 starved rescue; tool cuts are not
		// continued here (see the package comment).
		return nil, "", false
	}
	if req.MaxTokens < continuationMinBudget {
		return nil, "", false
	}
	why := ""
	longAnswer := false
	wasRaised := false
	floor := effortFloor(req)
	if floor > 0 && req.MaxTokens < floor {
		wasRaised = true
		why = fmt.Sprintf("budget was raised to the %s floor %d", strings.ToLower(req.RequestedEffort), floor)
	}
	if why == "" && ledger.thinking.Len() > 0 && ledger.thinking.Len() >= ledger.text.Len() {
		// Bytes stand in for the token ratio: this upstream reports no
		// reasoning_tokens. Think >= text ≈ thinking ate at least half of the
		// shared budget.
		why = "thinking dominates the answer"
	}
	if why == "" && req.MaxTokens >= clientLimitLongAnswer {
		// Signal 3 (misakano 6e7fe62): the budget is a client default, not an
		// answer spec. Unlike signals 1-2 this one does not need thinking to
		// be on — the cut is pure length, so a thinking-off client's answer
		// cut at 32000 continues just the same.
		longAnswer = true
		why = fmt.Sprintf("client budget %d is a long-answer default (>= %d)", req.MaxTokens, clientLimitLongAnswer)
	}
	if why == "" {
		return nil, "", false
	}
	if req.Thinking != nil && !req.Thinking.Enabled && !longAnswer {
		// Already off: for signals 1-2 a continuation cannot change anything
		// (the mechanism was freeing the budget FROM thinking). Signal 3
		// carries on regardless — the answer was cut by the number itself.
		return nil, "", false
	}
	capTokens := defaultContinueTotalCap
	if floor > 0 {
		capTokens = floor * continueTotalCapRatio
	}
	if longAnswer && capTokens < 2*clientLimitLongAnswer {
		// Our cap is CUMULATIVE across segments (the upstream's is
		// per-segment), so the plain floor×4 / 32768 default would stop a
		// 32000-budget continuation almost immediately. The measured natural
		// length (60660) fits under 2× the client default; keep the floor's
		// own cap when its ×4 already exceeds that.
		capTokens = 2 * clientLimitLongAnswer
	}
	if ledger.usage.OutputTokens >= capTokens {
		log.Printf("gateway: workbuddy cumulative output %d reached the continuation cap %d (floor %d × %d) — leaving the answer truncated at finish=length",
			ledger.usage.OutputTokens, capTokens, floor, continueTotalCapRatio)
		return nil, "", false
	}
	contReq := continuationRequest(req, ledger, false)
	contReq.Thinking = &ir.ThinkingConfig{Enabled: false}
	if req.Thinking != nil {
		// Keep Include semantics but disable the thinking itself (BP-112 same
		// shape): the point is to free the budget, not to hide output.
		contReq.Thinking.Include = req.Thinking.Include
	}
	// 关思考就别再带档位 (BP-112): a gear asks FOR thinking.
	contReq.RequestedEffort = ""
	if wasRaised && floor > contReq.MaxTokens {
		// e54c6e0: the rescue segment inherits the raised budget — carrying
		// the client's small value would just move the cut downstream.
		contReq.MaxTokens = floor
	}
	return contReq, why, true
}

// runLengthContinuation drives the same-account continuation loop and returns
// the merged response. Every segment splices into the client stream through
// emit; the terminal stays held until executeBinding releases it.
func runLengthContinuation(actx context.Context, binding router.Binding, lease *pool.Lease,
	contReq *ir.ChatRequest, client *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse,
	drv driver.Driver, emit driver.Emit, why string) *ir.ChatResponse {
	for seg := 1; seg <= lengthContMaxSegments; seg++ {
		if actx.Err() != nil {
			break
		}
		log.Printf("gateway: %s finish=length with a partial answer (%s) — continuing on the same account with thinking disabled (segment %d, %d chars so far)",
			binding.Channel, why, seg, ledger.text.Len())
		contResp, cerr := drv.Stream(actx, driver.ChatCall{
			Account: lease.Account, UpstreamModel: binding.UpstreamModel,
			Request: contReq,
		}, emit)
		if cerr != nil {
			// The partial answer already on the wire stays; the honest
			// finish=length terminal goes out below.
			log.Printf("gateway: length continuation failed (%v); the partial answer stays truncated", cerr)
			break
		}
		if contResp == nil {
			break
		}
		resp = mergedResponse(ledger, contResp)
		if contResp.StopReason != ir.StopLength {
			break
		}
		next, nextWhy, ok := lengthContinuationPlan(binding.Channel, client, ledger, resp)
		if !ok {
			break
		}
		contReq, why = next, nextWhy
	}
	return resp
}
