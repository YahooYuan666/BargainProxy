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
}

// Trace records how a request was attempted across bindings, so the request
// log can show cascades ("tried workbuddy then trae" = 2). Injected via
// context by the protocol handlers; nil-safe everywhere (tests and internal
// callers skip it).
type Trace struct {
	Attempts int
	Channels []string
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
	firstEvent := false
	seen := func(ev ir.StreamEvent) error {
		firstEvent = true
		return emit(ev)
	}
	emitted := func() bool { return firstEvent }
	for bi, binding := range bindings {
		// The window is per binding: slow-to-first-byte channels (AutoClaw's
		// sandbox relay) raise their own ceiling without making healthy quick
		// channels wait longer (BP-71).
		win := fbWaitFor(g.cfg.FallbackWait.D(), req, g.fbCeiling(binding.Channel))
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
				continue
			}
		}
		bctx := ctx
		var cancel context.CancelFunc
		// The sentinel guard is per-binding (its buffering state must not
		// leak across attempts), and the first-byte window wrapper sits
		// CLOSEST to the driver: any upstream output — including the first
		// text chunk the sentinel guard is still buffering — proves the
		// upstream started answering and must stop the window timer.
		guardSeen := sentinelGuard(seen, g.cfg.Sentinels)
		attemptEmit := guardSeen
		if bi < len(bindings)-1 {
			// First-byte window (ticket 28): bounds the wait for this binding
			// to START answering — not the whole generation. Drivers aggregate
			// the full stream inside the call, so a whole-attempt deadline
			// used to clip every generation longer than the window (glm-5.3 /
			// kimi-k3 lost each >8s response to the 8s fallback wait).
			var wcancel context.CancelFunc
			bctx, wcancel = context.WithCancel(ctx)
			cancel = wcancel
			timer := time.AfterFunc(win, wcancel)
			attemptEmit = func(ev ir.StreamEvent) error {
				timer.Stop()
				return guardSeen(ev)
			}
		}
		res, err := g.executeBinding(bctx, binding, req, attemptEmit, bi > 0, emitted, tr)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return res, nil
		}
		lastErr = err
		if ue, ok := uperr.As(err); ok && bi < len(bindings)-1 {
			if ue.Class == uperr.ClassCanceled || ue.Class == uperr.ClassUpstreamTimeout {
				log.Printf("gateway: %s first-byte window expired (%s); cascading", binding.Channel, win)
			}
		}
		if ctx.Err() != nil {
			return nil, err // parent context done: client gone / shutdown
		}
		ue, ok := uperr.As(err)
		if ok && !uperr.Rotatable(ue.Class) {
			// Timeout / cancellation may still try a different channel (the
			// next binding is a different upstream), but only while nothing
			// has reached the client (tickets 27/28).
			if (ue.Class == uperr.ClassUpstreamTimeout || ue.Class == uperr.ClassCanceled) && !(req.Stream && firstEvent) {
				continue
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
	return nil, lastErr
}

// estimatePromptTokens is a coarse size estimate (chars/3) used only to widen
// the first-byte window; exact accounting stays with the upstream usage.
func estimatePromptTokens(req *ir.ChatRequest) int {
	if req == nil {
		return 0
	}
	chars := 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Kind == "text" {
				chars += len(p.Text)
			}
		}
	}
	if chars == 0 {
		return 0
	}
	return chars / 3
}

// fbWindowCeiling is the global first-byte window cap: base plus 1s per 8k
// estimated prompt tokens can never exceed this — unless a channel raises its
// own ceiling via config (AutoClaw, BP-71).
const fbWindowCeiling = 30 * time.Second

// fbCeiling returns the first-byte window ceiling for one channel. AutoClaw's
// sandbox relay prefill on 300k-token ZCode contexts exceeds the global 30s
// cap (measured: cascades logged at exactly 30s on every long request while
// the relay was healthy), so the AutoClaw binding defaults to a wider ceiling
// and channels.autoclaw.first_byte_window can tune it (BP-71, user report
// 2026-10-17). Every other channel keeps the original 30s cap.
func (g *Gateway) fbCeiling(channel string) time.Duration {
	if channel == model.ChannelAutoClaw {
		if w := g.cfg.Channels.AutoClaw.FirstByteWindow.D(); w > 0 {
			return w
		}
		return defaultAutoClawFBWindow
	}
	return fbWindowCeiling
}

// defaultAutoClawFBWindow bounds how long the AutoClaw binding may take its
// first byte before the cascade moves on. The relay answers small requests
// in ~7s but needs long prefill on full ZCode sessions; the old global 30s
// cap killed every such request before the first byte arrived.
const defaultAutoClawFBWindow = 90 * time.Second

// fbWaitFor returns the first-byte window for this request: base plus one
// second per 8k estimated prompt tokens, capped at ceiling (global 30s unless
// the binding's channel raises it).
func fbWaitFor(base time.Duration, req *ir.ChatRequest, ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		ceiling = fbWindowCeiling
	}
	if base <= 0 {
		base = fallbackWait
	}
	n := estimatePromptTokens(req)
	if n <= 0 {
		return base
	}
	w := base + time.Duration(n/8000)*time.Second
	if w > ceiling {
		w = ceiling
	}
	return w
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

func (g *Gateway) executeBinding(ctx context.Context, binding router.Binding, req *ir.ChatRequest, emit driver.Emit, degraded bool, emitted func() bool, tr *Trace) (*Result, error) {
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
		lease, err := g.pool.Acquire(ctx, binding.Channel, req.Model, req.SessionKey)
		if err != nil {
			// Prefer the concrete failure from the previous attempt over the
			// pool-level "no account available": the client learns WHY (e.g.
			// "trae: refresh token invalid — re-login required") instead of a
			// retry-later 429 (ticket 37).
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, g.classifyPoolError(err, binding.Channel, req.Model)
		}
		call := driver.ChatCall{Account: lease.Account, UpstreamModel: binding.UpstreamModel, Request: req}
		// The sentinel guard (and the first-byte window wrapper) are already
		// composed by Execute — pass emit through unchanged.
		resp, streamErr := drv.Stream(ctx, call, emit)
		if streamErr == nil {
			g.pool.ApplyResult(lease.Account, req.Model, nil)
			// Record the model use so the cost observer can classify billing
			// tiers from balance deltas (credit channels only).
			if binding.Channel == model.ChannelWorkBuddy || binding.Channel == model.ChannelTrae {
				g.pool.NoteModelUse(lease.Account, binding.UpstreamModel)
			}
			g.persistCreds(lease.Account)
			res := &Result{Response: resp, Channel: binding.Channel, Account: lease.Account, Attempts: tr.Attempts}
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
		g.pool.ApplyResult(lease.Account, req.Model, ue)
		g.persistCreds(lease.Account)
		lease.Release()
		lastErr = streamErr
		if !uperr.Rotatable(ue.Class) {
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
			return ue
		}
		ue := uperr.New(uperr.ClassQuotaExhausted, 429, "no available account on channel %s", channel)
		ue.ResetAt = nc.EarliestReset
		return ue
	}
	return err
}

// persistCreds saves refreshed credentials through the store (best effort —
// a persistence hiccup must not fail a successful request).
func (g *Gateway) persistCreds(acc *model.Account) {
	if acc == nil || !acc.CredsDirty || g.store == nil {
		return
	}
	if err := g.store.SaveAccount(acc); err == nil {
		acc.CredsDirty = false
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
		msg := "all accounts are cooling down or exhausted"
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
		// Capacity event: the account is healthy and the request is fine;
		// 503 tells the client SDK to back off and retry shortly.
		return 503, ue.Class, "upstream queue full (capacity), retry shortly: " + ue.Message
	case uperr.ClassWAF:
		return 502, ue.Class, "upstream edge blocked the request: " + ue.Message
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
