// Package gateway orchestrates one client request: resolve model bindings,
// acquire an account lease, run the channel driver, fold results back into
// pool state, and rotate/fallback on classified failures.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// Gateway is the request orchestrator.
type Gateway struct {
	cfg     *config.Config
	pool    *pool.Pool
	router  *router.Router
	drivers map[string]driver.Driver
	store   store.Store
	// fbWindow is the panel-managed global first-byte ceiling (nanoseconds;
	// 0 = fall back to config/default). Hot-updated, no restart needed.
	fbWindow atomic.Int64
}

// New builds the gateway.
func New(cfg *config.Config, p *pool.Pool, r *router.Router, drivers map[string]driver.Driver, st store.Store) *Gateway {
	return &Gateway{cfg: cfg, pool: p, router: r, drivers: drivers, store: st}
}

// Result is the outcome of a successful request.
type Result struct {
	Response *ir.ChatResponse
	Channel  string
	Account  *model.Account
	Degraded string // set when fallback moved off the preferred binding
	// Attempts is the number of upstream/binding attempts this request paid
	// before succeeding (1 = no cascade). On failure the tally is read off
	// the request Trace instead (see TraceFrom).
	Attempts int
	// ToolsDeclared rides the request's tool count so the log row can record
	// it next to the calls actually made (BP-133).
	ToolsDeclared int
}

// Trace records how a request was attempted across bindings, so the request
// log can show cascades ("tried workbuddy then trae" = 2). Injected via
// context by the protocol handlers; nil-safe everywhere (tests and internal
// callers skip it).
type Trace struct {
	Attempts int
	Channels []string
	// LastAccount is the account of the most recent leg, success or failure.
	// Recorded so a FAILED request still names an account in the log: the
	// request row is built from the Result on success, and a failed turn used to
	// end up with an empty account column, leaving "which account dropped this
	// connection?" unanswerable (BP-107 field report, 2026-10-24).
	LastAccount *model.Account
	// BadRequestResent pins the BP-168 flaky-400 resend to once per REQUEST
	// (bindings included): a cascade that meets two lying 400s must not pay
	// two resends.
	BadRequestResent bool
	// EffortRetried pins the BP-182 effort-downgrade retry to once per
	// request.
	EffortRetried bool
	// SharedRateRodeOut pins the BP-221 shared-quota hold-and-resend to once
	// per request: a second FreeUsageLimitError inside the same turn falls
	// through to the pool's normal handling instead of stacking holds.
	SharedRateRodeOut bool
}

// BP-221 tunables: how long one shared-lane hold lasts and how many times a
// turn may ride the lease while the shared bucket is closed. Overridable in
// tests (sharedRateHold is shortened there).
var (
	sharedRateHold       = 50 * time.Second
	sharedRateMaxResends = 4
)

// effortRejected reports whether a 400 body names the effort gear as the
// thing it refused. Deliberately narrow (BP-182): a generic "invalid
// argument" must not trigger a downgrade.
func effortRejected(msg string) bool {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning.effort") {
		return true
	}
	return strings.Contains(lower, "effort") &&
		(strings.Contains(lower, "not support") || strings.Contains(lower, "unsupported") ||
			strings.Contains(lower, "invalid") || strings.Contains(lower, "reject"))
}

// downgradeEffort steps the gear down one level; the bottom of the ladder
// strips the gear entirely (the upstream's own default takes over). ok=false
// for gears with no lower step.
func downgradeEffort(effort string) (string, bool) {
	switch strings.ToLower(effort) {
	case "max":
		return "xhigh", true
	case "xhigh":
		return "high", true
	case "high":
		return "medium", true
	case "medium":
		return "low", true
	case "low", "minimal":
		return "", true
	}
	return "", false
}

type traceKey struct{}

// WithTrace attaches a Trace to ctx.
func WithTrace(ctx context.Context, tr *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, tr)
}

// TraceFrom returns the Trace attached to ctx, or nil.
func TraceFrom(ctx context.Context) *Trace {
	tr, _ := ctx.Value(traceKey{}).(*Trace)
	return tr
}

// note records one leg against a channel (same channel repeated for account
// rotations within a binding is collapsed).
func (t *Trace) note(channel string) {
	if t == nil {
		return
	}
	t.Attempts++
	if len(t.Channels) == 0 || t.Channels[len(t.Channels)-1] != channel {
		t.Channels = append(t.Channels, channel)
	}
}

// maxAccountAttempts bounds rotation within one binding.
const maxAccountAttempts = 3

// fallbackWait bounds how long one binding may hold a request before the
// cascade tries the next binding (applies when more bindings exist after the
// current one). Prevents a serial channel (Trae, concurrency=1) from making
// every request wait the full queue timeout while a sibling channel is idle.
const fallbackWait = 8 * time.Second

// Execute runs one request end to end. emit receives stream events (may be
// nil for non-streaming callers — the response is still aggregated).
//
// Failure semantics:
//   - content-level errors (bad request / blocked / sentinel) surface
//     immediately without rotating (rotating burns accounts on a payload
//     problem that will fail everywhere).
//   - account-level errors rotate to the next account in the binding, then to
//     the next binding (fallback), then surface.
func (g *Gateway) Execute(ctx context.Context, req *ir.ChatRequest, emit driver.Emit) (*Result, error) {
	if req.Stream && emit == nil {
		return nil, fmt.Errorf("gateway: stream requested but no emitter")
	}
	// The image model has no chat route on purpose (chat cannot carry image
	// parts back); point the caller at the images endpoint instead of a bare
	// "model not found" (BP-186).
	if gerr := g.imageModelGate(req.Model); gerr != nil {
		return nil, gerr
	}
	bindings := g.router.Resolve(req.Model)
	if len(bindings) == 0 {
		return nil, uperr.New(uperr.ClassBadRequest, 404, "model %q not found", req.Model)
	}
	// Skip bindings whose channel has no imported account at all: holding a
	// request for the fallback wait on a channel that can never serve it
	// (route configured before any account was imported) adds a silent 8s
	// delay per empty binding. A channel: escape hatch keeps its binding even
	// when empty — the user explicitly pinned it, so the Acquire fast-fail
	// (ErrNoCapacity) is the honest answer, not a silent reroute.
	bareModel, forcedChannel, hasPrefix := router.ParseChannelPrefix(req.Model)
	pinned := hasPrefix && forcedChannel != ""
	if !pinned {
		filtered := make([]router.Binding, 0, len(bindings))
		for _, b := range bindings {
			if g.pool.HasAccounts(b.Channel) {
				filtered = append(filtered, b)
			}
		}
		bindings = filtered
	}
	// Debug escape hatch: after the routed decision, strip the
	// "channel:<name>:" prefix so logs, the echoed response model and pool
	// cost keys all use the bare model name (the actual channel lands in the
	// request-log row).
	if hasPrefix && bareModel != "" {
		req.Model = bareModel
	}

	// Outbound normalization: merge the back-to-back assistant tool_calls
	// shape some clients replay — upstream DeepSeek-family models reject it
	// with 11148 and kill the session on every account (ticket 35,
	// agentpoints 0d0e509). Idempotent when the ingress already merged.
	req.Messages, _ = ir.MergeAdjacentToolCalls(req.Messages)
	// Strict upstreams (the workbuddy/zcode DeepSeek family) reject any history
	// whose assistant tool_calls and tool results do not pair up, with
	// code=11148 and no way to resume the conversation. Repair the pairing so
	// an interrupted turn or a replayed foreign history self-heals instead of
	// failing forever.
	if repaired, changed := ir.RepairToolPairing(req.Messages); changed {
		log.Printf("gateway: repaired tool call/result pairing (%d → %d messages)", len(req.Messages), len(repaired))
		req.Messages = repaired
	}

	// Output ceiling (BP-99): cap max_tokens at the model's real limit before
	// any driver sees it. Too large a value gets a 400 from some upstreams and
	// a SILENT truncation from others — and a silent truncation is the worse
	// outcome, because the client reads a short answer with no hint that the
	// gateway caused it. Clamped once here rather than per driver so every
	// channel benefits and no driver can forget.
	//
	// The upstream model id is the better key (a canonical name can differ from
	// what the channel calls it), but the binding is only known after routing,
	// so the canonical name is used here and the driver-level ceiling in
	// config covers the mapping.
	if req.MaxTokens > 0 {
		if capped, changed := config.ClampOutput(req.MaxTokens, req.Model, bareModel); changed {
			log.Printf("gateway: clamped max_tokens %d → %d for model %s (output ceiling)",
				req.MaxTokens, capped, req.Model)
			req.MaxTokens = capped
		}
	}

	var lastErr error
	// First-byte window: base from config, widened by context size. Large ZCode
	// conversations can spend >8s in prefill before the first token; a fixed
	// window cancelled a healthy workbuddy binding and cascaded to AutoClaw
	// (user report 2026-10-17). The widening ceiling is per channel: the
	// AutoClaw sandbox relay's prefill on 300k-token contexts routinely
	// exceeds the global 30s cap, so it raises its own ceiling via config
	// instead of every request cascading at 30s (BP-71).
	// Attempt tracing (BP-38): where a request can cascade across bindings,
	// the log must show how many legs it actually paid. Handlers inject a
	// Trace via context; when absent (tests, internal callers) a private one
	// still feeds Result.Attempts.
	tr := TraceFrom(ctx)
	if tr == nil {
		tr = &Trace{}
	}
	// firstEvent flips on the first emitted stream event. For streaming
	// clients that means bytes are already on the wire: a second attempt
	// (account rotation or another binding) would duplicate the output, so
	// cascading stops once it is set (tickets 27/28). Buffered requests keep
	// cascading — nothing reaches the client before the very end.
	// BP-107: the ledger records exactly what the client has seen, which is
	// what a mid-stream continuation has to replay. Recording happens HERE,
	// inside the sentinel guard's flush target, so text the guard is still
	// buffering (and may drop as a rejection) is never counted as delivered.
	ledger := &streamLedger{}
	// BP-226: lone-CR scrubber for streaming tool-argument deltas — one
	// scrubber per client request, covering every driver's tool stream.
	crArgs := &crTidy{}
	firstEvent := false
	seen := func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvToolArgs:
			ev.Delta = crArgs.write(ev.Delta)
		case ir.EvToolStart, ir.EvAttemptReset:
			// A new tool's args (or a fresh attempt) begins: a held \r was
			// the last byte of the previous stream — lone, dropped.
			crArgs.flush()
		}
		ledger.record(ev)
		// Only CLIENT-VISIBLE content counts as "the client has seen part of
		// the answer" (BP-118). Usage and the terminal event are bookkeeping:
		// an upstream that sends usage (or an empty done) and then dies has
		// produced nothing the client can read, so both remedies must stay
		// available — the cross-channel cascade (needs emitted()==false) and
		// the same-account continuation (needs text on the ledger).
		//
		// Field 2026-10-26 01:02: a workbuddy leg died with "stream read:
		// unexpected EOF" at 27s having emitted only bookkeeping. firstEvent
		// was already true, so the cascade was refused; the ledger had no
		// text, so canContinue() refused too. The request failed with both
		// escape hatches shut, while the same conversation answered 200 on the
		// same channel seconds before and after.
		switch ev.Kind {
		case ir.EvText, ir.EvThinking:
			firstEvent = true
		case ir.EvToolStart, ir.EvToolArgs:
			// Tool deltas count as delivered ONLY on protocols that stream
			// them as they arrive (OpenAI chat, Responses). Anthropic and
			// Gemini buffer tool blocks until the turn ends (BP-70 / finalize):
			// there a mid-tool drop has delivered NOTHING the client can read,
			// so the cross-channel cascade must stay available (BP-123).
			//
			// Field 2026-10-26 22:19: a workbuddy leg died with "stream read:
			// unexpected EOF" 92s in, having emitted ONLY tool-call fragments
			// (proof by elimination: ttfb=2295ms means an event was emitted;
			// no continuation log means the ledger had no text/thinking, so
			// the event was a tool one; attempts=1 means the cascade was
			// refused). firstEvent was true so the cascade was refused,
			// sawTool refused the continuation, and the client — which had
			// received nothing but message_start — lost the whole turn while
			// the same conversation answered 200 on the same channel before
			// and after. On the Anthropic ingress the tool fragments were
			// still sitting in the writer's buffer, undelivered.
			if !req.ToolDeltasBuffered {
				firstEvent = true
			}
		case ir.EvAttemptReset:
			// A retry is about to reuse this stream: the failed attempt's
			// buffered state is being discarded everywhere (ledger below,
			// sentinel guard, protocol writers). It says nothing about what
			// the client has SEEN — the reset only flows when nothing was
			// delivered, so firstEvent stays false and the retry proceeds.
		}
		return emit(ev)
	}
	emitted := func() bool { return firstEvent }
	busyCount := 0
	attempted := false
	for bi, binding := range bindings {
		// The window is per binding: slow-to-first-byte channels (AutoClaw's
		// sandbox relay) raise their own ceiling without making healthy quick
		// channels wait longer (BP-71).
		win := fbWaitFor(g.fbBaseFor(), req, g.fbCeiling(binding.Channel))
		// TEMP BP-218 diagnostics: the open-design memory legs outlived their
		// client's timeout with the window never firing. Log the resolved
		// window chain until the discrepancy is named, then remove.
		if strings.HasPrefix(req.Model, "open-design-byok/") {
			log.Printf("bp218-diag: binding=%s bi=%d/%d base=%s ceiling=%s win=%s stream=%v",
				binding.Channel, bi, len(bindings), g.fbBaseFor(), g.fbCeiling(binding.Channel), win, req.Stream)
		}
		if bi == len(bindings)-1 {
			// The LAST binding has nowhere to cascade to, so a first-byte
			// window can only hurt: it turns a slow-but-healthy upstream into a
			// failure while the request's own header/idle watchdogs (300s) are
			// the real safety net. This is what "widen AutoClaw" asks for —
			// AutoClaw is usually the last resort, and its sandbox relay can
			// legitimately prefill for minutes (2026-10-20).
			win = 0
		}
		// Single-active mode: if this pool already has an account interacting,
		// do not wait and do not use a second account — walk to the next pool.
		// When every pool answers busy, the request ends as 并行数量超发.
		if busy, who := g.pool.ChannelBusy(binding.Channel); busy {
			busyCount++
			lastErr = concurrencyError(binding.Channel, who)
			// The skip must leave a name in the trace (BP-110): a binding
			// skipped for being busy otherwise vanishes from attempt_channels,
			// and "why didn't it use workbuddy? it was fine" becomes
			// unanswerable — the pool WAS considered, it was full (field
			// 2026-10-25 14:56: one conversation's own earlier requests held
			// every slot, the chain read trae→lobster with no workbuddy at all).
			tr.note(binding.Channel)
			continue
		}
		// Pre-flight mirrors the single-active rule: if this pool is already
		// interacting, the preflight above reports "usable" (it only sees
		// cooldowns), the request would commit and then bounce off Acquire.
		// Skip here so the next binding gets tried (2026-10-21: the 11:06
		// deepseek request ended as concurrency_exceeded on WorkBuddy alone
		// although qoder was healthy).
		// The AutoClaw SANDBOX RELAY carries exactly one text message to its
		// agent and drops every other part: an image request there does not
		// fail, the model answers without the image, which the user reads as
		// "it recognized a different picture" (2026-10-20). The CN managed
		// gateway that now carries AutoClaw traffic is different: it takes
		// OpenAI image parts and answers correctly (verified 2026-10-21 with a
		// half-red/half-blue PNG: zai_glm-5.3-flash, zai_auto and
		// zaicoding_glm-5.3 all named both colours). So an image request may
		// use AutoClaw unless the relay is the path that would serve it.
		if binding.Channel == model.ChannelAutoClaw && ir.HasImages(req.Messages) && g.cfg.Channels.AutoClaw.RelayEnabled {
			lastErr = uperr.New(uperr.ClassBadRequest, 400,
				"autoclaw relay is text-only and cannot carry images; another channel is required")
			continue
		}
		// Pre-flight availability gate (BP-64): do not queue on a channel
		// that has nothing usable right now when another binding can serve.
		// Structural blocks (empty pool, all disabled/paused, credit floor)
		// and cool-downs longer than the first-byte window are skipped
		// immediately with the pool's own error; a reset inside the window is
		// still attempted, so a short cooldown is honored. A pinned channel is
		// always attempted — the user asked for its honest answer.
		if !pinned {
			ok, earliest := g.pool.NextAvailable(binding.Channel, req.Model)
			if skipUnavailable(ok, earliest, time.Now(), win) {
				lastErr = g.classifyPoolError(&pool.ErrNoCapacity{
					Channel: binding.Channel, EarliestReset: earliest,
				}, binding.Channel, req.Model)
				// Same as the busy skip above: a binding skipped in pre-flight
				// must still be named in the trace (BP-110), or the chain lies
				// by omission about what was considered.
				tr.note(binding.Channel)
				continue
			}
		}
		bctx := ctx
		// The sentinel guard is per-binding (its buffering state must not
		// leak across attempts). The first-byte window is passed INTO
		// executeBinding and starts only once the pool hands out an account
		// lease: it used to wrap the whole binding attempt, so a request queued
		// behind a busy account (max_in_flight) burned its entire window
		// waiting for a slot and cascaded without the upstream ever seeing it.
		guardSeen := sentinelGuard(seen, g.cfg.Sentinels)
		attempted = true
		res, err := g.executeBinding(bctx, binding, req, guardSeen, win, bi > 0, emitted, tr, ledger)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err // parent context done: client gone / shutdown
		}
		ue, ok := uperr.As(err)
		if ok && !uperr.Rotatable(ue.Class) {
			// Non-rotatable means "do not try another account of THIS channel",
			// not "give up on every channel". A different channel is a
			// different upstream and may well have the route:
			//   - timeout / cancellation: the next upstream may be healthy;
			//   - 404: this binding's path/model is missing, another provider
			//     may still serve the same canonical model (2026-10-19);
			//   - content-blocked: the rejection is THIS upstream's content
			//     inspection, not a property of the request everywhere — a
			//     ZCode refusal must hand the turn to WorkBuddy instead of
			//     ending it as 400 (2026-10-21 field bug: a single-account
			//     ZCode pool made "do not rotate within the pool" read as
			//     "the request is dead", so glm-5.3-flash never reached the
			//     healthy bindings behind it).
			//   - bad-request: one upstream's strictness about a history shape
			//     is not the request being globally invalid — the same session
			//     answered 200 on qoder seconds before trae rejected it with
			//     "the param is invalid" (msg_5cdb126709, 2026-10-21). Cascading
			//     lets qoder finish the turn.
			// All are only safe while nothing reached the client.
			switch ue.Class {
			case uperr.ClassUpstreamTimeout, uperr.ClassCanceled, uperr.ClassNotFound,
				uperr.ClassContentBlocked, uperr.ClassSentinel, uperr.ClassBadRequest,
				uperr.ClassAccountRestricted:
				// BP-146: a restricted account means "this ACCOUNT is banned
				// upstream", not "the request is bad". Never retry another
				// account of the same channel (each attempt leaves a risk
				// record and the next account gets banned too — Coding2API
				// f428e33), but a different channel is a different upstream
				// and may well serve the turn.
				if !(req.Stream && firstEvent) {
					continue
				}
			}
			return nil, err
		}
		if req.Stream && firstEvent {
			// Mid-stream failure: re-running another binding would duplicate
			// content — surface the error instead.
			return nil, err
		}
		continue
	}
	if lastErr == nil {
		lastErr = uperr.New(uperr.ClassInternal, 0, "no binding attempted")
	}
	if !attempted && busyCount > 0 {
		// Every pool was already interacting; nothing was even dispatched.
		// Say it plainly: this is a parallel-capacity refusal, not a broken
		// upstream and not "no account available".
		return nil, uperr.New(uperr.ClassConcurrencyExceeded, 429,
			"并行数量超发：模型 %s 的所有号池当前都在处理其他请求（同时只允许一个账号交互），请稍后重试", req.Model)
	}
	return nil, lastErr
}

// concurrencyError renders the per-pool refusal. The gateway aggregates these
// into the final 并行数量超发 error when no pool could take the request.
func concurrencyError(channel, who string) *uperr.Error {
	msg := "号池 " + channel + " 已有账号在交互"
	if who != "" {
		msg += "（" + who + "）"
	}
	ue := uperr.New(uperr.ClassConcurrencyExceeded, 429, "%s；同时只允许一个账号交互", msg)
	ue.Channel = channel
	return ue
}

// estimatePromptTokens forwards to ir.EstimatePromptTokens (moved there for
// BP-103: the lobster driver gates its context ceiling with the same
// estimate, and drivers must not import the gateway package).
func estimatePromptTokens(req *ir.ChatRequest) int {
	return ir.EstimatePromptTokens(req)
}

// fbWindowCeiling is the default first-byte window cap for every channel
// (BP-72).
//
// BP-115 lowered it 90s → 30s. The 90s value was chosen on 2026-10-18 for
// AutoClaw's sandbox relay and then made global because "a saturated uplink
// slows the first byte for all of them". That reasoning was sound but the
// cost was not counted: the window is spent PER BINDING, so a request that
// cascades through five pools could sit for 5 x 90s = 7.5 minutes before the
// client saw anything (field 2026-10-25 23:14: attempts=5,
// zcode→autoclaw→workbuddy→qoder→trae, 187-306s, three legs timing out on
// the window). A slow uplink is a real cause, but the cure for "this leg will
// never answer" must stay cheap — 30s per leg bounds a full cascade at 2.5
// minutes.
//
// AutoClaw keeps its own longer ceiling through channels.autoclaw.first_byte_window
// (90s): its relay genuinely prefills for minutes on large contexts, and it
// sits last in every route, so its window is never multiplied by a cascade.
//
// BP-118: the flat 30s cap was rejecting requests that WOULD have answered.
// A 33万-token prompt spent 31.9s in workbuddy prefill and returned 200
// (2026-10-26 00:50:21); minutes later the same shape was cancelled at
// exactly 30.000s. The formula already widens with context (base + 2k
// tokens/s), so the CAP — not the formula — was doing the rejecting.
//
// The default cap is now 60s: enough for roughly a 1M-token prefill at the
// same 2k/s rate, while a five-leg cascade still costs at most 5 minutes
// (the 90s value it replaced allowed 7.5). Explicit per-channel and global
// settings are still honoured exactly as configured — an operator who set a
// number meant it.
//
// Note the cheap legs bound the cascade in practice: a prompt too big for a
// channel fails in milliseconds there (trae's 262144 tool ceiling rejects in
// 0s, the lobster byte gate in 18ms), so only the legs that can actually
// serve the request ever spend their window.
const (
	fbWindowCeiling     = 60 * time.Second
	fbWindowCeilingHard = 120 * time.Second
)

// fbCeiling returns the first-byte window CEILING for one channel, most
// specific first: channel_first_byte_windows[channel] → legacy
// channels.autoclaw.first_byte_window → fbWindowCeiling.
//
// BP-138: the panel's first_byte_window_seconds is deliberately NOT in this
// chain any more — it is the BASE of the dynamic formula (fbBaseFor), not a
// cap. Its old place here is exactly what kept amputating big prompts: the
// DB value 30 clamped the formula back down no matter what the context
// needed. An operator who wants a different ceiling for a channel still has
// channel_first_byte_windows; the global default remains fbWindowCeiling.
func (g *Gateway) fbCeiling(channel string) time.Duration {
	if g.cfg != nil {
		if w := g.cfg.ChannelFirstByteWindows[channel].D(); w > 0 {
			return w
		}
		if channel == model.ChannelAutoClaw {
			if w := g.cfg.Channels.AutoClaw.FirstByteWindow.D(); w > 0 {
				return w
			}
		}
	}
	return fbWindowCeiling
}

// SetFirstByteWindow hot-updates the global first-byte ceiling (panel setting).
// Per-channel overrides still win; 0 restores the config/default value.
func (g *Gateway) SetFirstByteWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	g.fbWindow.Store(int64(d))
}

// fbWaitFor returns the first-byte window for this request.
//
// BP-138 made the window DYNAMIC in earnest. The formula existed since BP-72
// (base + 1s per 2k estimated prompt tokens; the step was narrowed 8k → 4k →
// 2k after the 15:00 incident), but the panel value sat on top as a FLAT cap —
// and field 2026-10-27 20:20 proved the cap was still doing the rejecting: a
// 56.8万-token prompt needs ~50s of workbuddy prefill (33万 measured 31.9s,
// BP-118), the DB panel value 30 cut it off at exactly 30.000s, and the
// request cascaded through trae (context ceiling) and qoder (queue) to
// lobster, which answered three empty 500s. Every leg died of the size the
// window refused to acknowledge.
//
// The panel setting (first_byte_window_seconds) is therefore the BASE of the
// formula, not the cap: window = clamp(base + tokens/2k, base, hard ceiling).
// A small request keeps the operator's number exactly; a big one gets the
// prefill time the measured rate says it needs. fbWindowCeilingHard (120s)
// bounds a pathological prompt; the per-leg cascade cost is bounded by the
// cheap-legs observation below, not by starving healthy legs.
//
// 2k tokens/s is a realistic prefill rate under a congested uplink (BP-72
// measurement); requests that answer quickly never wait — the first stream
// event stops the timer.
func fbWaitFor(base time.Duration, req *ir.ChatRequest, ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		ceiling = fbWindowCeiling
	}
	if base <= 0 {
		base = fallbackWait
	}
	n := estimatePromptTokens(req)
	if n <= 0 {
		// A request whose text we cannot estimate must still honour the
		// channel ceiling: returning the UNCAPPED base is how a 10s
		// per-channel cap silently became 30s for exactly the requests we
		// understand least (field 2026-10-31: the open-design memory legs
		// ran past their client's timeout with the window never firing —
		// the 300ms live probe proved the whole mechanism, so the only
		// window value that fits the logs is the uncapped base).
		if base > ceiling {
			return ceiling
		}
		return base
	}
	w := base + time.Duration(n/2000)*time.Second
	if w > ceiling {
		w = ceiling
	}
	return w
}

// fbBaseFor resolves the BASE window for one channel: the panel value is the
// operator's base (BP-138), falling back to the config default, then to
// fallbackWait. Per-channel overrides remain what they always were — an
// explicit ceiling for a channel whose prefill rate differs.
func (g *Gateway) fbBaseFor() time.Duration {
	if n := g.fbWindow.Load(); n > 0 {
		return time.Duration(n)
	}
	if g.cfg != nil {
		if w := g.cfg.FallbackWait.D(); w > 0 {
			return w
		}
	}
	return fallbackWait
}

// skipUnavailable is the pre-flight gate predicate (BP-64): a binding with
// nothing usable right now is skipped unless its earliest known recovery falls
// inside the first-byte window (then the bounded wait is worth it). Structural
// blocks (zero earliest) are always skipped.
func skipUnavailable(ok bool, earliest, now time.Time, window time.Duration) bool {
	if ok {
		return false
	}
	if earliest.IsZero() {
		return true
	}
	return earliest.Sub(now) > window
}

func (g *Gateway) executeBinding(ctx context.Context, binding router.Binding, req *ir.ChatRequest, emit driver.Emit, window time.Duration, degraded bool, emitted func() bool, tr *Trace, ledger *streamLedger) (*Result, error) {
	drv, ok := g.drivers[binding.Channel]
	if !ok {
		return nil, uperr.New(uperr.ClassInternal, 0, "channel %q driver not installed", binding.Channel)
	}
	var lastErr error
	// Attempt accounting is per binding ("leg"), not per account rotation:
	// the question the log answers is "how many channels did this request
	// pay before succeeding?" (BP-38). Account rotations inside one binding
	// do not inflate the number.
	tr.note(binding.Channel)
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		// BP-129: a ceiling an upstream stated in a 400 binds this channel+model
		// until it expires — later requests clamp upfront instead of paying the
		// 400 + repaired retry again. The channel is only known here (routing
		// happens above; the ClampOutput spot runs pre-routing).
		if req.MaxTokens > 0 {
			if ceil := config.LearnedOutputCeiling(binding.Channel, req.Model); ceil > 0 && req.MaxTokens > ceil {
				log.Printf("gateway: clamped max_tokens %d → %d for %s/%s (learned ceiling)",
					req.MaxTokens, ceil, binding.Channel, req.Model)
				req.MaxTokens = ceil
			}
		}
		// Queue wait is deliberately OUTSIDE the first-byte window: Acquire
		// blocks while every account of the pool is at max_in_flight, and that
		// wait says nothing about how slow the upstream's first byte is. The
		// window starts below, right before the driver call, so a pooled
		// request that waited 25s for a slot still gets the full prefill
		// budget (2026-10-18 cascade: WorkBuddy/AutoClaw looked dead because
		// their window expired while queued behind busy accounts).
		lease, err := g.pool.Acquire(ctx, binding.Channel, req.Model, req.SessionKey)
		if err != nil {
			// Prefer the concrete failure from the previous attempt over the
			// pool-level "no account available": the client learns WHY (e.g.
			// "trae: refresh token invalid — re-login required") instead of a
			// retry-later 429 (ticket 37).
			if lastErr != nil {
				return nil, lastErr
			}
			cerr := g.classifyPoolError(err, binding.Channel, req.Model)
			// Pool-level failures never reach a driver, so they have no
			// "leg failed" line — log them too, or a binding that was skipped
			// for lack of capacity looks like it never existed.
			if ue, ok := uperr.As(cerr); ok {
				log.Printf("gateway: %s leg failed before dispatch: class=%s: %.160s",
					binding.Channel, ue.Class, ue.Message)
			}
			return nil, cerr
		}
		call := driver.ChatCall{Account: lease.Account, UpstreamModel: binding.UpstreamModel, Request: req}
		// Remember whose leg this is, so a failure can still be attributed.
		if tr != nil {
			tr.LastAccount = lease.Account
		}
		// First-byte window (ticket 28): bounds the wait for this attempt to
		// START answering — not the whole generation. Timer starts HERE, after
		// the lease, and is stopped by the first emitted event (the sentinel
		// guard may still be buffering that event, so this wrapper stays
		// outside it).
		actx := ctx
		var cancel context.CancelFunc
		attemptEmit := emit
		if window > 0 {
			actx, cancel = context.WithCancel(ctx)
			// TEMP BP-218 diagnostics: the timer's own fire must be visible —
			// a leg that outlives its window with no "window expired" note
			// could mean the timer never fired OR that the abort was ignored.
			timer := time.AfterFunc(window, func() {
				log.Printf("bp218-diag: WINDOW FIRED channel=%s win=%s", binding.Channel, window)
				cancel()
			})
			attemptEmit = func(ev ir.StreamEvent) error {
				timer.Stop()
				return emit(ev)
			}
		}
		started := time.Now()
		// BP-123: a RETRY is about to reuse the same client stream — another
		// account of this binding (attempt > 0) or the next binding after a
		// cascade (degraded). The failed attempt may have left buffered state
		// behind: tool fragments in the protocol writer (Anthropic/Gemini hold
		// them until the turn ends), un-matched text in the sentinel guard,
		// the ledger's tool flag. EvAttemptReset tells every layer to drop it,
		// or the next attempt's tool arguments would concatenate onto the dead
		// attempt's partial JSON. It only ever flows when nothing was
		// delivered — a delivered stream never reaches a retry.
		if degraded || attempt > 0 {
			_ = emit(ir.StreamEvent{Kind: ir.EvAttemptReset})
		}
		resp, streamErr := drv.Stream(actx, call, attemptEmit)
		// max_tokens floor repair (BP-99): some upstreams reject a too-small
		// max_tokens with a 400 whose text carries the minimum they demand
		// ("max_tokens: 1 is less than the minimum of 16"). A client sending
		// max_tokens=1 as a liveness probe therefore looks like a broken
		// gateway. Recover the number and retry ONCE on the same account with a
		// legal value — nothing reached the client yet, because a bad_request
		// means the turn never started.
		if streamErr != nil && !emitted() {
			if ue, ok := uperr.As(streamErr); ok && ue.Class == uperr.ClassBadRequest {
				if floor := config.FloorFromMessage(ue.Message); floor > 0 &&
					(floor > req.MaxTokens || req.MaxTokens <= 0) {
					raised := *req
					raised.MaxTokens = floor
					retryCall := call
					retryCall.Request = &raised
					log.Printf("gateway: %s rejected max_tokens=%d, retrying with %d (upstream minimum)",
						binding.Channel, req.MaxTokens, floor)
					started = time.Now()
					resp, streamErr = drv.Stream(actx, retryCall, attemptEmit)
				} else if ceil := config.CeilingFromMessage(ue.Message); ceil > 0 && req.MaxTokens > ceil {
					// BP-129: the mirror repair — "Range of max_tokens should
					// be [1, N]" names the model's real output maximum. Clamp,
					// retry once on the same account (a bad_request means the
					// turn never started), and remember for later turns.
					config.LearnOutputCeiling(binding.Channel, req.Model, ceil)
					clamped := *req
					clamped.MaxTokens = ceil
					retryCall := call
					retryCall.Request = &clamped
					log.Printf("gateway: %s rejected max_tokens=%d, retrying with %d (upstream maximum, learned)",
						binding.Channel, req.MaxTokens, ceil)
					started = time.Now()
					resp, streamErr = drv.Stream(actx, retryCall, attemptEmit)
				} else if (tr == nil || !tr.EffortRetried) && effortRejected(ue.Message) && req.RequestedEffort != "" {
					// BP-182 (Coding2API 90a5d6f5): an upstream that refuses
					// the effort gear downgrades ONE level on the SAME
					// account instead of failing the turn — no rotation, no
					// cooldown, no budget. Narrow match: only bodies that
					// name the gear. Nothing reached the client (outer
					// guard), so a clean retry is safe.
					if tr != nil {
						tr.EffortRetried = true
					}
					stepped, ok := downgradeEffort(req.RequestedEffort)
					if ok {
						downgraded := *req
						downgraded.RequestedEffort = stepped
						retryCall := call
						retryCall.Request = &downgraded
						log.Printf("gateway: %s rejected reasoning_effort=%q — retrying once with %q (same account)",
							binding.Channel, req.RequestedEffort, map[bool]string{true: stepped, false: "no gear"}[stepped != ""])
						started = time.Now()
						resp, streamErr = drv.Stream(actx, retryCall, attemptEmit)
					}
				} else if want := tokenFieldWanted(ue.Message); want != "" && req.MaxTokens > 0 {
					// BP-233: the refusal names the token-budget field it
					// wants ("Use 'max_completion_tokens' instead") — swap
					// once on the same account. Nothing reached the client
					// (outer guard), and a vendor that names the fix in its
					// own words has told us the body was otherwise fine.
					current := "max_tokens"
					if req.MaxCompletionTokens {
						current = "max_completion_tokens"
					}
					if want != current {
						swapped := *req
						swapped.MaxCompletionTokens = want == "max_completion_tokens"
						retryCall := call
						retryCall.Request = &swapped
						log.Printf("gateway: %s asked for %s instead of %s — retrying once with the field swapped",
							binding.Channel, want, current)
						started = time.Now()
						resp, streamErr = drv.Stream(actx, retryCall, attemptEmit)
					}
				} else if (tr == nil || !tr.BadRequestResent) && req.Stream {
					// BP-168: upstreams do emit LYING 400s. Field evidence
					// 2026-10-28 23:46: Google's antigravity answered a generic
					// INVALID_ARGUMENT to the 54th request of a healthy
					// session — structurally identical to the 52 requests
					// before it (dump-diffed: delta was one clean tool
					// result), and the same content flowed fine 30 seconds
					// later in a fresh session. bad_request never rotates, so
					// one flaky 400 killed a whole 5-minute subagent run.
					// Resend the IDENTICAL bytes to the SAME account once per
					// request: a deterministic 400 costs 3 seconds, a flaky
					// one gets through. Only while nothing reached the client
					// (the outer guard) and only on streaming turns, where
					// "no output yet" is meaningful.
					//
					// The AttemptReset first: whatever the dead attempt left
					// buffered (sentinel hold-back, protocol writer state)
					// must be discarded, or it would leak into the resent
					// stream (BP-123 discipline; nothing was delivered, which
					// is exactly this branch's guard).
					if tr != nil {
						tr.BadRequestResent = true
					}
					_ = emit(ir.StreamEvent{Kind: ir.EvAttemptReset})
					log.Printf("gateway: %s returned bad_request with nothing emitted — resending once to the same account (flaky-400 guard)",
						binding.Channel)
					started = time.Now()
					resp, streamErr = drv.Stream(actx, call, attemptEmit)
				}
			}
		}
		// BP-169 (user option A, misakano f07eba54): queue signals (6020/6021)
		// are INTERMITTENT — field evidence 2026-10-28: two accounts answered
		// 6021 and the same model recovered within 33s, so fail-fast turned a
		// few-second wobble into a dead task. Retry the SAME lease with a
		// short backoff (500ms, 1s — 3 upstream calls total), only while
		// nothing reached the client; a queue naming a wait longer than 5s is
		// too deep to ride out here and goes straight to the terminal error.
		// No account rotation, no penalty (the pool's channel+model pause
		// still applies to follow-up requests).
		if streamErr != nil && !emitted() {
			if ue, ok := uperr.As(streamErr); ok && ue.Class == uperr.ClassUpstreamBusy && ctx.Err() == nil {
				for r := 0; r < 2; r++ {
					if !ue.ResetAt.IsZero() {
						if wait := time.Until(ue.ResetAt); wait > 5*time.Second {
							break // deep queue: hand the wait to the client
						}
					}
					backoff := 500 * time.Millisecond << r
					select {
					case <-actx.Done():
					case <-time.After(backoff):
					}
					if actx.Err() != nil {
						break
					}
					_ = emit(ir.StreamEvent{Kind: ir.EvAttemptReset})
					log.Printf("gateway: %s queue busy — retrying on the same account in %s (attempt %d/3)",
						binding.Channel, backoff, r+2)
					started = time.Now()
					resp, streamErr = drv.Stream(actx, call, attemptEmit)
					if streamErr == nil {
						break
					}
					next, ok := uperr.As(streamErr)
					if !ok || next.Class != uperr.ClassUpstreamBusy {
						break
					}
					ue = next
				}
			}
		}
		// BP-221: the zen free lane's FreeUsageLimitError is EPISODIC. Field
		// 2026-10-31: one request in a burst of healthy ones was refused with
		// the shared-allowance 429, and the identical shape answered 200 seven
		// more times over the following hour (8/8 at a 10s pace at peak); the
		// allowance belongs to the egress (SharedQuota), so the wall is never
		// the account's. Ride it out: hold the same lease ~45s and resend the
		// identical bytes once, while nothing reached the client. A real
		// exhaustion survives this once and then falls through to the pool's
		// short shared-quota rest and the honest error.
		if streamErr != nil && !emitted() {
			if ue, ok := uperr.As(streamErr); ok && ue.Class == uperr.ClassSoftRate &&
				ue.SharedQuota && ctx.Err() == nil && (tr == nil || !tr.SharedRateRodeOut) {
				if tr != nil {
					tr.SharedRateRodeOut = true
				}
				// BP-221 (upgraded 2026-10-31 night): the lane's shared bucket
				// refills on its own schedule — minutes of silence did not
				// reopen it, yet the same bytes passed earlier the same hour —
				// so a single 45s hold is not patient enough. Ride the lease:
				// up to sharedRateMaxResends holds ~50s apart, each resend with
				// a fresh first-byte window, only while nothing reached the
				// client and the user is still connected. A turn then costs
				// minutes instead of failing; a real day-long exhaustion still
				// lands as the honest error.
				for r := 0; r < sharedRateMaxResends && ctx.Err() == nil; r++ {
					wait := sharedRateHold
					if !ue.ResetAt.IsZero() {
						if d := time.Until(ue.ResetAt); d > 0 && d < wait {
							wait = d
						}
					}
					log.Printf("gateway: %s shared free lane refused — holding the lease %s (resend %d/%d, BP-221)",
						binding.Channel, wait.Round(time.Second), r+1, sharedRateMaxResends)
					// The hold runs on the CLIENT context: the failed
					// attempt's first-byte timer is still armed and would cut
					// the hold mid-way (the 8s-window test proved it).
					select {
					case <-ctx.Done():
					case <-time.After(wait):
					}
					if ctx.Err() != nil {
						break
					}
					_ = emit(ir.StreamEvent{Kind: ir.EvAttemptReset})
					started = time.Now()
					resendCtx := ctx
					resendEmit := emit
					if window > 0 {
						var resendCancel context.CancelFunc
						resendCtx, resendCancel = context.WithCancel(ctx)
						timer := time.AfterFunc(window, resendCancel)
						defer resendCancel()
						resendEmit = func(ev ir.StreamEvent) error {
							timer.Stop()
							return emit(ev)
						}
					}
					resp, streamErr = drv.Stream(resendCtx, call, resendEmit)
					if streamErr == nil {
						break
					}
					next, ok := uperr.As(streamErr)
					if !ok || next.Class != uperr.ClassSoftRate || !next.SharedQuota {
						break // a different failure: hand it to the normal path
					}
					ue = next
				}
			}
		}
		// BP-107 mid-stream continuation: the client has already seen part of
		// the answer and the upstream dropped the connection. Switching channel
		// is off the table (it would replay the whole turn), so carry on with
		// the SAME account: replay what the client saw as an assistant turn,
		// add a "carry on" instruction, and splice the new events into the same
		// stream. See continue.go for why each guard exists.
		//
		// Streaming only: a buffered client has nothing on the wire, so the
		// ordinary cross-channel cascade is strictly better than re-asking the
		// same account.
		contSegs := 0
		for streamErr != nil && req.Stream && ctx.Err() == nil && contSegs < continueMaxSegments {
			ue, _ := uperr.As(streamErr)
			class := uperr.ClassInternal
			if ue != nil {
				class = ue.Class
			}
			if !ledger.canContinue(class, contSegs) {
				break
			}
			contSegs++
			log.Printf("gateway: %s stream dropped after %s (%s); continuing on the same account (segment %d/%d, %d chars replayed%s)",
				binding.Channel, time.Since(started).Round(time.Millisecond), class,
				contSegs, continueMaxSegments, ledger.text.Len(),
				map[bool]string{true: ", redoing the interrupted tool call", false: ""}[ledger.sawTool])
			// Decide the redo BEFORE the restart event clears the ledger
			// flag: continuationRequest reads the same state.
			redoTool := ledger.sawTool
			if redoTool {
				// BP-139: the truncation cut a tool call mid-arguments. The
				// partial fragment is discarded everywhere (writers retire
				// the buffered block and shift their index space; the ledger
				// clears the flag) and the continuation request asks the
				// model to redo the call. The client — on buffered
				// protocols — never saw the fragment, so what it experiences
				// is one continuous stream whose tool call simply arrives
				// complete.
				if err := attemptEmit(ir.StreamEvent{Kind: ir.EvToolRestart}); err != nil {
					return nil, err
				}
			}
			var contResp *ir.ChatResponse
			contReq := continuationRequest(req, ledger, redoTool)
			contResp, streamErr = drv.Stream(actx, driver.ChatCall{
				Account: lease.Account, UpstreamModel: binding.UpstreamModel,
				Request: contReq,
			}, attemptEmit)
			if streamErr == nil {
				resp = mergedResponse(ledger, contResp)
			}
		}
		elapsed := time.Since(started)
		if cancel != nil {
			cancel()
		}
		// Starved-thinking rescue (BP-112): a turn that spent its WHOLE output
		// budget on reasoning (finish=length, no text, no tools, no surfaced
		// thinking) gets ONE retry with thinking off at the SAME budget. The
		// retry reclaims the budget FROM thinking; raising it would overspend
		// what the client asked. See starved.go for every guard.
		if retryReq, starved := starvedRetryRequest(req, resp); starved {
			log.Printf("gateway: %s turn ended finish=length with no output at max_tokens=%d — thinking consumed the budget; retrying once with thinking disabled",
				binding.Channel, req.MaxTokens)
			retryCall := call
			retryCall.Request = retryReq
			startedRetry := time.Now()
			if retryResp, rerr := drv.Stream(actx, retryCall, attemptEmit); rerr == nil && retryResp != nil &&
				!(retryResp.StopReason == ir.StopLength && ir.TextOf(retryResp.Message.Content) == "") {
				resp = retryResp
				log.Printf("gateway: starved-thinking rescue answered (stop=%s, %s)",
					retryResp.StopReason, time.Since(startedRetry).Round(time.Millisecond))
			} else if rerr != nil {
				// The rescue failing is not the client's problem: the original
				// empty answer already says finish=length honestly.
				log.Printf("gateway: starved-thinking rescue failed (%v); returning the original answer", rerr)
			}
		}
		if streamErr == nil {
			// BP-185 (Coding2API 03d408e4): text volume far beyond the
			// reported output tokens is the indirect signature of a swallowed
			// usage frame or a silently served smaller model. Warn only.
			if resp != nil && resp.Usage.OutputTokens > 0 {
				if chars := ledger.text.Len(); chars > 200 && chars/resp.Usage.OutputTokens > 12 {
					log.Printf("gateway: usage reconciliation: %s/%s produced %d text chars but reported %d output tokens — upstream may have swallowed usage or served a smaller model",
						binding.Channel, binding.UpstreamModel, chars, resp.Usage.OutputTokens)
				}
			}
			g.pool.ApplyResult(lease.Account, req.Model, nil)
			// Tell the pool what this answer said about the vendor's prefix
			// cache: stickiness is only worth keeping while that cache is warm
			// (BP-95).
			if resp != nil {
				g.pool.NoteSessionCache(binding.Channel, req.SessionKey, resp.Usage)
			}
			// Record the model use so the cost observer can classify billing
			// tiers from balance deltas (credit channels only).
			if binding.Channel == model.ChannelWorkBuddy || binding.Channel == model.ChannelTrae {
				g.pool.NoteModelUse(lease.Account, binding.UpstreamModel)
			}
			// Qoder reports the request's own credits on every answer
			// (BP-98 Qoder-1): feed the cost observer directly — 0 credits is
			// a free model (tier 0), a positive amount is paid (tier 2).
			// Without this the pool read every Qoder model as "unknown" and
			// free-first routing plus the exhausted-balance floor guard
			// never fired for the channel.
			if binding.Channel == model.ChannelQoder && resp != nil && resp.Usage.CreditKnown {
				tier := 2
				if resp.Usage.Credit == 0 {
					tier = 0
				}
				g.pool.RecordCostTier(lease.Account, binding.UpstreamModel, tier)
			}
			g.persistCreds(lease.Account)
			// BP-226: the aggregated arguments are the authoritative copy on
			// buffered protocols and the one replayed into continuations —
			// scrub lone CRs here too, after the delta stream already was.
			for i := range resp.Message.ToolCalls {
				resp.Message.ToolCalls[i].Arguments = dropLoneCR(resp.Message.ToolCalls[i].Arguments)
			}
			res := &Result{Response: resp, Channel: binding.Channel, Account: lease.Account, Attempts: tr.Attempts, ToolsDeclared: len(req.Tools)}
			if degraded {
				res.Degraded = binding.Channel
			}
			lease.Release()
			return res, nil
		}
		ue, ok := uperr.As(streamErr)
		if !ok {
			ue = uperr.New(uperr.ClassInternal, 0, "%v", streamErr)
		}
		// One line per failed leg: without it the log only shows the final
		// channel's error, which is how "WorkBuddy died" rumors start. The
		// window marker separates "our timer gave up" from an upstream error.
		//
		// The account is named too (BP-107): a failed leg used to log only the
		// channel, so "which account dropped this connection?" could not be
		// answered from the logs at all — the request row has no account on a
		// failed request either, and the only remaining clue was the upstream IP
		// in the error text.
		winNote := ""
		if window > 0 && ue.Class == uperr.ClassCanceled && ctx.Err() == nil {
			winNote = fmt.Sprintf(" first-byte window %s expired", window)
			// BP-217: this cancel came from OUR timer — the client is still
			// connected and the upstream sent zero bytes for a whole window.
			// Record it so a second stall on the same channel+model pauses the
			// pair and the following requests cascade without re-paying the
			// window on a silently hung upstream. A client cancel (ctx.Err()
			// != nil) never reaches this branch, so ticket-27's "cancellations
			// carry no penalty" keeps holding.
			g.pool.NoteWindowExpiry(binding.Channel, req.Model)
		}
		// The HTTP status is printed too (BP-110): several drivers word an
		// upstream failure as "<channel>: <body>" and truncate the body, so a
		// non-200 with an EMPTY body logged as `class=transport: lobster: ` —
		// with nothing to tell a 500 from a 502, or an empty body from a
		// driver bug that produced no message. The status is the one fact that
		// survives every wording, so it goes in the line unconditionally
		// (status=0 means the request never got a response).
		log.Printf("gateway: %s account %s (%s) leg failed after %s: class=%s status=%d%s: %.160s",
			binding.Channel, lease.Account.ID, lease.Account.Nickname,
			elapsed.Round(time.Millisecond), ue.Class, ue.Status, winNote, ue.Message)
		g.pool.ApplyResult(lease.Account, req.Model, ue)
		if ue.Class == uperr.ClassAccountRestricted {
			// Say out loud that an account just left routing: otherwise the
			// only trace is a disabled checkbox in the panel, and "the pool is
			// broken" is indistinguishable from "two accounts got banned".
			log.Printf("gateway: %s account %s (%s) disabled — upstream restricted it; re-login required: %.120s",
				binding.Channel, lease.Account.ID, lease.Account.Nickname, ue.Message)
		}
		g.persistCreds(lease.Account)
		lease.Release()
		lastErr = streamErr
		if !uperr.Rotatable(ue.Class) {
			// Do not rotate accounts within THIS binding — but a downstream
			// channel is a different upstream whose content inspection may
			// accept what this one rejected. Return to the binding loop, which
			// decides between cascade and surface (2026-10-21, same contract
			// as the loop-level non-rotatable branch).
			return nil, streamErr
		}
		if ctx.Err() != nil {
			return nil, streamErr
		}
		if req.Stream && emitted() {
			// Events already reached the client: rotating to another account
			// would duplicate the output mid-stream (ticket 28) — stop.
			return nil, streamErr
		}
	}
	return nil, lastErr
}

func (g *Gateway) classifyPoolError(err error, channel, modelName string) error {
	// Race path: the channel turned busy between the pre-flight check and the
	// lease. Same meaning — another pool may still take the request.
	var cb *pool.ErrChannelBusy
	if errors.As(err, &cb) {
		return concurrencyError(cb.Channel, cb.Account)
	}
	// Dead model: several distinct accounts were told the upstream does not
	// serve it. Answer as "no such model on this channel" so the binding loop
	// moves on instead of walking more accounts (2026-10-22).
	var mg *pool.ErrModelGone
	if errors.As(err, &mg) {
		ue := uperr.New(uperr.ClassModelUnavailable, 404,
			"%s does not serve model %s (%d accounts confirmed): %s",
			mg.Channel, mg.Model, mg.Accounts, mg.Reason)
		ue.Model = mg.Model
		ue.Channel = mg.Channel
		return ue
	}
	var nc *pool.ErrNoCapacity
	if errors.As(err, &nc) {
		// Distinguish "every account is model-cooled for this model" from
		// "the pool has no usable account": the client's remedy differs
		// (switch models vs retry) — ticket 33, agentpoints 44d6e04.
		if blocked, until, count := g.pool.ModelBlocked(channel, modelName); blocked {
			ue := uperr.New(uperr.ClassModelUnavailable, 400,
				"model %s is cooling down on every %s account (%d accounts); switch models or retry later",
				modelName, channel, count)
			ue.ResetAt = until
			ue.Model = modelName
			ue.Channel = channel
			return ue
		}
		ue := uperr.New(uperr.ClassQuotaExhausted, 429, "no available account on channel %s", channel)
		ue.ResetAt = nc.EarliestReset
		ue.Channel = channel
		return ue
	}
	return err
}

// persistCreds saves refreshed credentials through the store (best effort —
// a persistence hiccup must not fail a successful request).
func (g *Gateway) persistCreds(acc *model.Account) {
	if acc == nil || !acc.CredentialsDirty() || g.store == nil {
		return
	}
	raw := acc.Credentials()
	if err := g.store.SaveAccount(acc); err == nil {
		acc.CredentialsPersisted(raw)
	}
}

// ApplyProbe folds a driver probe result into pool state.
func (g *Gateway) ApplyProbe(acc *model.Account, pr *driver.ProbeResult) {
	if pr == nil {
		return
	}
	if len(pr.Windows) > 0 {
		g.pool.RecordWindows(acc, pr.Windows)
	}
	if pr.Credits != nil {
		g.pool.RecordCredits(acc, *pr.Credits)
	}
}

// LogRequest persists one request audit row (metadata only).
func (g *Gateway) LogRequest(l *model.RequestLog) {
	if g.store == nil {
		return
	}
	if l.Time.IsZero() {
		l.Time = time.Now()
	}
	_ = g.store.AppendRequestLog(l)
}

// ModelList returns the canonical models that currently resolve, with their
// ordered channel bindings (for /v1/models and the panel).
type ModelInfo struct {
	Model    string         `json:"model"`
	Channels []ModelChannel `json:"channels"`
}

// ModelChannel is one binding rendered for clients.
type ModelChannel struct {
	Channel string `json:"channel"`
	Native  string `json:"native_model"`
}

// ListModels lists canonical models with bindings.
func (g *Gateway) ListModels() []ModelInfo {
	primary := map[string]bool{}
	for _, m := range g.cfg.PrimaryModels {
		primary[config.NormalizeModel(m)] = true
	}
	var out []ModelInfo
	for _, name := range g.router.KnownModels() {
		if len(primary) > 0 && !primary[name] {
			continue // display curation only; routing unaffected
		}
		mi := ModelInfo{Model: name}
		for _, b := range g.router.Resolve(name) {
			mi.Channels = append(mi.Channels, ModelChannel{Channel: b.Channel, Native: b.UpstreamModel})
		}
		out = append(out, mi)
	}
	// The text-to-image model is not a chat route but belongs in the list:
	// image clients discover it here (and the a2-gemini skill probes for it).
	// Only listed while the channel can actually serve it — a model nobody
	// can call is noise, not discovery (BP-186).
	if g.ImageGenAvailable() && g.pool.HasAccounts(imageChannel) {
		seen := false
		for _, mi := range out {
			if strings.EqualFold(mi.Model, model.ImageGenModel) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, ModelInfo{Model: model.ImageGenModel, Channels: []ModelChannel{
				{Channel: imageChannel, Native: model.ImageGenModel},
			}})
		}
	}
	return out
}

// RouteRows returns the configured route table for the panel editor.
func (g *Gateway) RouteRows() []router.Row {
	rows := g.router.Rows()
	primary := map[string]bool{}
	for _, m := range g.cfg.PrimaryModels {
		primary[config.NormalizeModel(m)] = true
	}
	if len(primary) == 0 {
		return rows
	}
	filtered := make([]router.Row, 0, len(rows))
	for _, r := range rows {
		if primary[config.NormalizeModel(r.Model)] {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// ReplaceRoutes hot-swaps the model route table (validated by the router).
func (g *Gateway) ReplaceRoutes(bindings []config.ModelBinding) error {
	return g.router.Replace(bindings)
}

// RouteRowsAll returns the FULL route table with no primary_models
// filtering (BP-218). RouteRows curates what the panel DISPLAYS — its own
// config comment promises "routing is unaffected". Anything that feeds a
// whole-table WRITE-BACK must use this instead: zen's route apply sourced
// the filtered view, so within one sync tick it rewrote the live table and
// the DB without every route row outside the operator's primary list
// (field 2026-10-31: the open-design-byok/deepseek-v4.1-flash alias was
// applied at boot — "routes=40 applied" in the log — and answering 404
// two minutes later, because the zen write-back had already dropped it).
func (g *Gateway) RouteRowsAll() []router.Row {
	return g.router.Rows()
}

// Driver exposes a driver for probe tasks (panel/tasks).
func (g *Gateway) Driver(channel string) driver.Driver { return g.drivers[channel] }

// Pool exposes the pool (panel/tasks).
func (g *Gateway) Pool() *pool.Pool { return g.pool }

// SetWorkBuddyGlobal toggles the workbuddy international accounts switch
// (default off: global accounts stay out of scheduling).
func (g *Gateway) SetWorkBuddyGlobal(enabled bool) {
	g.pool.SetGlobalEnabled(enabled)
}

// SetGreedyFloor hot-updates the greedy burn-rate floor.
func (g *Gateway) SetGreedyFloor(d time.Duration) { g.pool.SetGreedyFloor(d) }

// SetSoftCooldownBase hot-updates the soft-rate cooldown base.
func (g *Gateway) SetSoftCooldownBase(d time.Duration) { g.pool.SetSoftCooldownBase(d) }

// SetChannelLimit hot-updates a channel's per-account in-flight limit.
func (g *Gateway) SetChannelLimit(channel string, n int) { g.pool.SetLimit(channel, n) }

// CostTier reports the freshest observed cost tier for a native model
// (0 free, 2 paid). ok=false when unobserved.
func (g *Gateway) CostTier(native string) (int, bool) { return g.pool.ModelCostTier(native) }

// promptReplacer is implemented by drivers with a hot-switchable system
// prompt replacement.
type promptReplacer interface{ SetPromptReplaceEnabled(bool) }

// SetPromptReplace toggles the workbuddy system-prompt replacement at runtime.
func (g *Gateway) SetPromptReplace(enabled bool) {
	if d, ok := g.drivers[model.ChannelWorkBuddy].(promptReplacer); ok {
		d.SetPromptReplaceEnabled(enabled)
	}
}

// thoughtSigReplayer is implemented by drivers with the experimental
// real-thought-signature replay cache (BP-44, antigravity).
type thoughtSigReplayer interface{ SetThoughtSignatureReplay(bool) }

// SetThoughtSignatureReplay toggles the experimental real-signature replay on
// the antigravity driver (panel hot setting; off by default).
func (g *Gateway) SetThoughtSignatureReplay(enabled bool) {
	if d, ok := g.drivers[model.ChannelAntigravity].(thoughtSigReplayer); ok {
		d.SetThoughtSignatureReplay(enabled)
	}
}

// Store exposes the store (panel/tasks).
func (g *Gateway) Store() store.Store { return g.store }

// Config exposes the live config (read-only use).
func (g *Gateway) Config() *config.Config { return g.cfg }

// DescribeError renders a user-facing message for an upstream error.
func DescribeError(err error) (status int, class string, message string) {
	ue, ok := uperr.As(err)
	if !ok {
		return 500, uperr.ClassInternal, "internal error"
	}
	switch ue.Class {
	case uperr.ClassQuotaExhausted:
		// Name the pool: this is the LAST binding's error after the earlier
		// bindings failed, and the old wording made one exhausted channel read
		// as "every account in every pool is frozen" (2026-10-18 user report).
		msg := "all accounts are cooling down or exhausted"
		if ue.Channel != "" {
			msg += " on " + ue.Channel
		}
		if !ue.ResetAt.IsZero() {
			msg = fmt.Sprintf("%s; earliest reset at %s", msg, ue.ResetAt.Format(time.RFC3339))
		}
		return 429, ue.Class, msg
	case uperr.ClassSoftRate:
		return 429, ue.Class, "upstream rate limited: " + ue.Message
	case uperr.ClassModelRate:
		// Model-scoped upstream rate limit (workbuddy 6004: "您的使用量已超出
		// 频率限制，将在 … 重置"): rate limiting by nature, so answer 429 and
		// name the model + reset so the client can back off or switch (BP-47).
		// Before this case it fell through to the default branch, where the
		// classifier's status 200 clamped to 500 — an honest upstream rate
		// limit looked like a gateway bug in both the client and the panel.
		msg := "upstream model rate limit"
		if ue.Model != "" {
			msg += " on " + ue.Model
		}
		if !ue.ResetAt.IsZero() {
			msg += "; resets at " + ue.ResetAt.Format(time.RFC3339)
		}
		// The hold is capped (BP-49): name the gateway's own next attempt when
		// it precedes the upstream reset, so a client backoff lands there.
		if !ue.HoldUntil.IsZero() && (ue.ResetAt.IsZero() || ue.HoldUntil.Before(ue.ResetAt)) {
			msg += "; gateway retries from " + ue.HoldUntil.Format(time.RFC3339)
		}
		return 429, ue.Class, msg + ": " + ue.Message
	case uperr.ClassHardCredit:
		return 429, ue.Class, "upstream credits exhausted: " + ue.Message
	case uperr.ClassAuth:
		return 401, ue.Class, "upstream authentication failed: " + ue.Message
	case uperr.ClassBadRequest:
		if ue.Status == 404 {
			return 404, ue.Class, ue.Message
		}
		return 400, ue.Class, ue.Message
	case uperr.ClassContentBlocked, uperr.ClassSentinel:
		return 400, ue.Class, "request rejected by upstream content policy"
	case uperr.ClassModelUnavailable:
		msg := ue.Message
		if !ue.ResetAt.IsZero() {
			msg = fmt.Sprintf("%s; earliest unlock at %s", msg, ue.ResetAt.Format(time.RFC3339))
		}
		return 400, ue.Class, msg
	case uperr.ClassTransport:
		return 502, ue.Class, "upstream transport failure: " + ue.Message
	case uperr.ClassUpstreamTimeout:
		// Distinguishable from "no available account": the client's remedy is
		// to retry later, not to re-route — and the pool was not penalized
		// (ticket 27).
		return 503, ue.Class, "upstream stalled or timed out (account not penalized; another account would hit the same slow upstream): " + ue.Message
	case uperr.ClassCanceled:
		// The attempt was aborted by cancellation (client gone / fallback
		// window). Typically the response never reaches a client that is
		// still listening.
		return 499, ue.Class, "request canceled before completion: " + ue.Message
	case uperr.ClassUpstreamBusy:
		// Capacity event: the account is healthy and the request is fine.
		// BP-169 (user option A): 429 + Retry-After, not 503 — the WorkBuddy
		// CLI resends automatically on a 429's Retry-After and ABORTS the
		// whole task on a 503 (misakano f07eba54); Anthropic-family SDKs
		// behave the same way. When the upstream said how long the queue
		// needs, say it (the header is attached by withRetryAfter from
		// ResetAt/HoldUntil; the 5s default and 600s cap live there).
		if !ue.ResetAt.IsZero() {
			if wait := time.Until(ue.ResetAt); wait > 0 {
				return 429, ue.Class, fmt.Sprintf("upstream queue is full; retry in %s: %s",
					wait.Round(time.Second), ue.Message)
			}
		}
		return 429, ue.Class, "upstream queue full (capacity), retry shortly: " + ue.Message
	case uperr.ClassWAF:
		return 502, ue.Class, "upstream edge blocked the request: " + ue.Message
	case uperr.ClassEdgeBlock:
		// A CDN/WAF edge refused the request before it reached the vendor.
		// Same client-facing story as the WAF case, but no account was
		// charged and the channel was paused instead (BP-94).
		return 502, ue.Class, "upstream edge blocked the request (request-level, no account affected): " + ue.Message
	case uperr.ClassNotFound:
		// The upstream has no such path/model. The client's remedy is to fix
		// the model name (or the gateway's route table), so answer the honest
		// 404 instead of a 429/500 that suggests "retry" or "gateway bug".
		// Every channel was already tried when nothing had reached the client.
		return 404, ue.Class, "upstream has no route for this model: " + ue.Message
	case uperr.ClassAccountRestricted:
		// The account was banned by upstream and has left routing. Answer 403
		// (not 500): the client is not at fault, and retrying the same account
		// is pointless — the panel now shows the disabled account.
		return 403, ue.Class, "upstream restricted the account: " + ue.Message
	case uperr.ClassConcurrencyExceeded:
		// Every pool is serving someone else. 429 + the plain reason; queueing
		// silently was the old behaviour and it looked like a hang.
		return 429, ue.Class, ue.Message
	default:
		status := ue.Status
		if status < 400 || status > 599 {
			status = 500
		}
		return status, ue.Class, ue.Message
	}
}

// HasUnsupportedImageURL detects image URLs that policy forbids fetching.
func HasUnsupportedImageURL(req *ir.ChatRequest) (string, bool) {
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Kind == "image_url_unsupported" {
				return p.Text, true
			}
		}
	}
	return "", false
}

// TrimModelName guards against absurd model strings.
func TrimModelName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// BareModel strips the "channel:<name>:" debug prefix for logging (the
// request-log row records the bare model name plus the actual channel).
func BareModel(s string) string {
	if bare, _, has := router.ParseChannelPrefix(s); has && bare != "" {
		return bare
	}
	return s
}
