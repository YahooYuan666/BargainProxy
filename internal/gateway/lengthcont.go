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
// Scope: workbuddy (the three signals above) and — since BP-317 — trae, on a
// DIFFERENT trigger: trae's SOLO upstream cuts a segment at a per-model
// output cap and reports finish_reason:"stop" (traework2api-panel fe0b0cdb,
// field-measured 2026-11-10: deepseek-v4.1-flash cuts at exactly 32000,
// glm-5.3__max at 16000, with or without a client max_tokens). The cut is
// disguised as a clean end; the trigger is "stop + pure prose + output >= the
// model's measured cap". Other channels bill thinking separately (raising or
// continuing there would rewrite the client's own semantics); a length cut
// inside a tool call is the BP-139 redo shape and needs its own evidence
// before it earns a slot.
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

// traeCutCaps: per-model single-segment output caps on the trae SOLO
// upstream, measured in the field (traework2api-panel fe0b0cdb, 2026-11-10).
// The upstream cuts at the cap and reports finish_reason:"stop" — the
// truncation is disguised as a clean end, with or without a client
// max_tokens. ONLY measured caps go in the table (no guessing); the plateau
// WARN inside traeCutContinuationPlan grows it from live traffic. Keys are
// the trae UPSTREAM model names (config model_routes upstream_model).
var traeCutCaps = map[string]int{
	"deepseek-v4.1-flash": 32000,
}

// traeCutPlateau: the smallest output at which an exact multiple of 1000 is
// treated as a suspected cap (twp fe0b0cdb's capPlateau — natural endings
// almost never land exactly on a round thousand).
const traeCutPlateau = 8000

// lengthContinuationPlan decides whether a finished turn with a partial prose
// answer continues on the same account. channel picks the trigger family:
// workbuddy = finish=length signals 1-3, trae = the disguised-cut cap table
// (BP-317). It returns the rewritten request (thinking off, raised budget
// inherited) and a human reason for the log.
func lengthContinuationPlan(channel, upstreamModel string, req *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse) (*ir.ChatRequest, string, bool) {
	if req == nil || resp == nil || ledger == nil {
		return nil, "", false
	}
	if channel == model.ChannelTrae {
		return traeCutContinuationPlan(upstreamModel, req, ledger, resp)
	}
	if channel != model.ChannelWorkBuddy {
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

// traeCutContinuationPlan (BP-317) is the trae twin of the length path: the
// upstream cuts a segment at a per-model output cap while reporting
// finish_reason:"stop" (traework2api-panel fe0b0cdb field data). Trigger:
// stop + pure prose + cumulative output >= the model's measured cap. Every
// gate is fail-closed; the plateau WARN below is the table's growth mechanism,
// not a rescue.
func traeCutContinuationPlan(upstreamModel string, req *ir.ChatRequest, ledger *streamLedger, resp *ir.ChatResponse) (*ir.ChatRequest, string, bool) {
	if resp == nil || ledger == nil {
		return nil, "", false
	}
	if resp.StopReason != ir.StopEnd {
		// An honest finish=length on trae has no measured mechanism yet — the
		// workbuddy signals (shared thinking budget, raised floors) do not
		// transfer, and inventing one here would rewrite the client's own
		// semantics. Leave it truncated.
		return nil, "", false
	}
	if ledger.sawTool || ledger.text.Len() == 0 {
		return nil, "", false
	}
	out := ledger.usage.OutputTokens
	name := strings.ToLower(strings.TrimSpace(upstreamModel))
	cap, known := traeCutCaps[name]
	if !known || out < cap {
		// Plateau detector (twp fe0b0cdb): the upstream cuts on round
		// thousand-token boundaries; a natural ending almost never lands
		// exactly there. An ending at a plateau below the registered cap is
		// the same smell with a smaller number.
		if out >= traeCutPlateau && out%1000 == 0 {
			log.Printf("WARN: gateway: trae output %d on %q ends cleanly on stop — suspected unregistered cut cap (if confirmed in the field, add it to traeCutCaps)", out, upstreamModel)
		}
		return nil, "", false
	}
	// Same cumulative spending gate as the workbuddy path: a continuation may
	// not push the turn past floor × 4 (or the 32768 default).
	floor := effortFloor(req)
	capTokens := defaultContinueTotalCap
	if floor > 0 {
		capTokens = floor * continueTotalCapRatio
	}
	if ledger.usage.OutputTokens >= capTokens {
		log.Printf("gateway: trae cumulative output %d reached the continuation cap %d (floor %d × %d) — leaving the answer truncated at the cut cap",
			ledger.usage.OutputTokens, capTokens, floor, continueTotalCapRatio)
		return nil, "", false
	}
	contReq := continuationRequest(req, ledger, false)
	contReq.Thinking = &ir.ThinkingConfig{Enabled: false}
	if req.Thinking != nil {
		// Keep Include semantics but disable the thinking itself (BP-112 same
		// shape): the whole segment budget goes to prose, so a re-cut lands
		// on prose length, not thinking.
		contReq.Thinking.Include = req.Thinking.Include
	}
	contReq.RequestedEffort = ""
	if contReq.MaxTokens < cap {
		// The client's budget (if any) may be smaller than the model's own
		// cap — carrying it would re-cut the segment early. The cap is the
		// smallest budget that survives one full segment with thinking off.
		contReq.MaxTokens = cap
	}
	return contReq, fmt.Sprintf("output %d hit the trae cut cap %d (upstream reports it as stop)", out, cap), true
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
		log.Printf("gateway: %s turn cut mid-answer (%s) — continuing on the same account with thinking disabled (segment %d, %d chars so far)",
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
		next, nextWhy, ok := lengthContinuationPlan(binding.Channel, binding.UpstreamModel, client, ledger, resp)
		if !ok {
			break
		}
		contReq, why = next, nextWhy
	}
	return resp
}
