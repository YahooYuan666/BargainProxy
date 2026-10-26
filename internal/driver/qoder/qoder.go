// Package qoder implements the Alibaba Qoder channel: PAT onboarding, the
// COSY-signed chat protocol, live model catalogue, quota probe and daily
// check-in.
//
// The wire-level protocol files (crypto.go/chat.go/stream.go) are ported from
// the agentpoints2 reference (MIT). This file adapts that engine to the
// gateway's driver contract: no HTTP hop, no in-process account pool — the
// gateway pool owns scheduling and this driver owns the upstream session.
package qoder

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

const (
	busyCode = "10605"

	// Queue-wait contract (2026-10-21, qoder2api-hub #38/#57 parity): a 403 +
	// {"code":"10605","isQueued":true,"retryAfterSeconds":N} answer is the
	// upstream's QUEUEING PROTOCOL, not a rate limit — the official client
	// sleeps N seconds and retries the same request, with a total budget up to
	// an hour. Our previous handling (fixed 2s, one retry) turned every queue
	// round into a client-visible failure, which is why the
	// deepseek-v4.1-flash p1 binding kept failing. Defaults are the
	// reference's conservative budget: 3 rounds, 180s per round, 600s total,
	// plus 0–2s jitter so simultaneous wakeups do not stampede the queue.
	// Queue budget (2026-10-21 revision): the gateway now waits at most
	// queueWaitBudget in total, then hands the answer back to the client as
	// "busy, retry in N seconds". Measured queue behaviour: p50 3.3s, p95
	// 12.9s, tail up to 63s on very large sessions — so 30s covers >95% of
	// real waits while a parked minute-long request no longer looks like a
	// freeze. Reference gateways return 503 + Retry-After instead of holding
	// the connection (magpie-community/plugins packages/qoder).
	queueMaxRounds   = 3
	queueWaitMax     = 30 * time.Second
	queueWaitBudget  = 30 * time.Second
	defaultQueueWait = 30 * time.Second

	pathJobExchange = "/api/v1/jobToken/exchange"
	pathJobRefresh  = "/api/v1/jobToken/refresh"
	pathCheckStatus = "/sash/api/v1/me/daily-check-in/status"
	pathCheckClaim  = "/sash/api/v1/me/daily-check-in/claim"
	pathProElig     = "/sash/api/v1/me/pro-upgrade/eligibility"
	pathProClaim    = "/sash/api/v1/me/pro-upgrade/claim"
)

// Creds is the persisted PAT credential blob.
type Creds struct {
	PAT          string `json:"pat"`
	Name         string `json:"name"`
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
}

// ParseCreds decodes our canonical keys (and tolerates personal_token).
func ParseCreds(raw []byte) (*Creds, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("creds: %w", err)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
		return ""
	}
	c := &Creds{
		PAT:          get("pat", "personal_token", "personalToken"),
		Name:         get("name", "nickname"),
		MachineID:    get("machine_id", "machineId"),
		MachineToken: get("machine_token", "machineToken"),
		MachineType:  get("machine_type", "machineType"),
	}
	if strings.TrimSpace(c.PAT) == "" {
		return nil, fmt.Errorf("pat missing")
	}
	return c, nil
}

// Marshal encodes the canonical credential blob.
func (c *Creds) Marshal() []byte {
	raw, _ := json.Marshal(c)
	return raw
}

// runtime is the in-memory session state for one account. Sessions are derived
// from the PAT, so a restart simply re-exchanges.
type runtime struct {
	mu            sync.Mutex
	renewing      bool
	creds         Creds
	uid           string
	userType      string
	securityOauth string
	refreshToken  string
	expireTimeMs  int64
	refreshedAt   time.Time
	bootstrapped  bool
	sess          *sessionContext
	// sessions carries the BP-111 aggregation state: stable per-conversation
	// session ids and per-turn task tuples. Lives per account with the rest
	// of the runtime.
	sessions *sessionTable

	bearerTok   string
	bearerExpMs int64
}

// Driver implements driver.Driver for Qoder.
type Driver struct {
	cfg  config.QoderSettings
	http *http.Client // chat streams
	rpc  *http.Client // short signed calls

	mu sync.Mutex
	rt map[string]*runtime

	catMu sync.Mutex
	cat   *ModelCatalog
	catAt time.Time

	// Device-fingerprint salt (see machineSalt): generated once, persisted in
	// the driver's data dir, never changed afterwards.
	saltOnce sync.Once
	saltVal  string
}

// New builds the driver.
func New(cfg config.QoderSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	return &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient(), rt: map[string]*runtime{}}
}

func (d *Driver) gatewayBase() string {
	if d.cfg.GatewayBase != "" {
		return strings.TrimRight(d.cfg.GatewayBase, "/")
	}
	return authBase
}

func (d *Driver) openAPIBase() string {
	if d.cfg.OpenBase != "" {
		return strings.TrimRight(d.cfg.OpenBase, "/")
	}
	return openBase
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelQoder }

// Models implements driver.Driver: the static catalogue (the live list needs
// an account, which this interface does not carry).
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	keys := make([]string, 0, len(DefaultModelMap))
	for k := range DefaultModelMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// Stream implements driver.Driver.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	rt, err := d.runtimeFor(call.Account)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "qoder: %v", err)
	}
	var lastErr error
	// 10605 is the upstream's QUEUEING PROTOCOL, not a rate limit: the answer
	// carries retryAfterSeconds and the official client waits before resending
	// the SAME request. Two mechanisms, in order (BP-92):
	//
	//   1. Queue-status polling: ask /ask/queue/status with the request's
	//      request_set_id until the queue says the request is no longer
	//      queued, then resend with that same request_set_id — that identity
	//      is what makes Qoder treat the resend as the same queued request
	//      rather than a new one at the back of the line.
	//   2. If the status endpoint is missing or unreadable, fall back to the
	//      previous behaviour: sleep retryAfterSeconds and resend.
	//
	// Both are bounded by queueWaitBudget so a gateway request never parks for
	// an hour; past the budget the answer goes back to the client as a busy
	// signal with the wait the upstream asked for.
	queueRounds := 0
	var queueSpent time.Duration
	requestSetID := ""
	for attempt := 0; attempt < 1+queueMaxRounds; attempt++ {
		if err := d.ensureFreshSession(ctx, rt); err != nil {
			return nil, qoderError(err)
		}
		var usedSetID string
		res, emitted, err := d.streamOnce(ctx, rt, call, emit, requestSetID, &usedSetID)
		if err == nil {
			return res, nil
		}
		lastErr = err
		ue, _ := uperr.As(err)
		if ue == nil || emitted {
			return nil, err
		}
		switch {
		case ue.Class == uperr.ClassAuth && attempt == 0:
			if d.forceRefresh(ctx, rt) == nil {
				continue
			}
		case ue.Class == uperr.ClassSoftRate && strings.Contains(ue.Message, busyCode):
			wait := queueWaitSeconds(ue.Message)
			if wait <= 0 {
				return nil, err
			}
			if queueRounds >= queueMaxRounds || queueSpent+wait > queueWaitBudget {
				// Budget exhausted: hand the queue answer back to the client as
				// a busy signal WITH the wait the upstream asked for. The
				// gateway stops holding the connection (which is what the user
				// experiences as a freeze) and the client decides whether to
				// retry, switch model or give up.
				busy := uperr.New(uperr.ClassUpstreamBusy, 503,
					"qoder: queued, ask again in %s (%s)", wait.Round(time.Second), clip(ue.Message, 80))
				busy.ResetAt = time.Now().Add(wait)
				return nil, busy
			}
			// The resend must carry the same request_set_id the queued request
			// used (or, if the body named one, that one).
			if id := queueRequestSetID(ue.Message); id != "" {
				requestSetID = id
			} else {
				requestSetID = usedSetID
			}
			queueRounds++
			state, haveState := parseQueueState([]byte(extractJSONObject(ue.Message)))
			polled := false
			rt.mu.Lock()
			sess := rt.sess
			rt.mu.Unlock()
			if haveState && requestSetID != "" && sess != nil {
				deadline := time.Now().Add(minDuration(wait, queuePollBudget))
				if d.awaitQueue(ctx, sess, state, requestSetID, deadline) {
					polled = true
				} else if ctx.Err() != nil {
					return nil, qoderError(ctx.Err())
				} else {
					logQueueFallback("queue not ready or status unreadable")
				}
			}
			if !polled {
				// Fallback: the plain sleep the previous behaviour used.
				queueSpent += wait
				jitter := time.Duration(randIntN(2000)) * time.Millisecond
				log.Printf("qoder: upstream queued (%s) — waiting %s (round %d/%d, spent %s)",
					clip(ue.Message, 60), wait.Round(time.Second), queueRounds, queueMaxRounds,
					queueSpent.Round(time.Second))
				select {
				case <-ctx.Done():
					return nil, qoderError(ctx.Err())
				case <-time.After(wait + jitter):
				}
			}
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (d *Driver) streamOnce(ctx context.Context, rt *runtime, call driver.ChatCall, emit driver.Emit, requestSetID string, usedSetID *string) (*ir.ChatResponse, bool, error) {
	cat := d.getCatalog(ctx, rt)
	body, display, qoderKey, toolsEnabled, setId, err := d.buildChatBody(call, rt, cat, requestSetID)
	if err != nil {
		return nil, false, err
	}
	if usedSetID != nil {
		*usedSetID = setId
	}
	encoded, err := gatewayEncode(body)
	if err != nil {
		return nil, false, uperr.New(uperr.ClassInternal, 0, "qoder: encode body: %v", err)
	}
	fullURL := d.gatewayBase() + chatPath
	date := time.Now().Unix()
	rt.mu.Lock()
	sess := rt.sess
	rt.mu.Unlock()
	if sess == nil {
		return nil, false, uperr.New(uperr.ClassAuth, 0, "qoder: no session")
	}
	bearer := sess.buildBearer(date, encoded, sigPath(fullURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(encoded))
	if err != nil {
		return nil, false, err
	}
	req.Header = bearerHeaders(sess, date, bearer, "text/event-stream")
	req.Header.Set("x-model-key", qoderKey)
	req.Header.Set("x-model-source", "system")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, false, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, false, classifyQoder(resp.StatusCode, string(raw))
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()

	reqID := "chatcmpl-" + newUUID()[:24]
	created := time.Now().Unix()
	var (
		textBuf   strings.Builder
		thinkBuf  strings.Builder
		tools     = map[int]*ir.ToolCall{}
		toolOrder []int
		emitted   bool
		emitErr   error
		usageObj  *usage
	)
	acc := newStreamAccumulator(reqID, created, display, toolsEnabled, func(chunk string) {
		if emitErr != nil {
			return
		}
		payload := strings.TrimSpace(strings.TrimPrefix(chunk, "data: "))
		var c struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &c) != nil {
			return
		}
		for _, ch := range c.Choices {
			if ch.Delta.ReasoningContent != "" {
				thinkBuf.WriteString(ch.Delta.ReasoningContent)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ch.Delta.ReasoningContent}); err != nil {
					emitErr = err
					return
				}
				emitted = true
			}
			if ch.Delta.Content != "" {
				textBuf.WriteString(ch.Delta.Content)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ch.Delta.Content}); err != nil {
					emitErr = err
					return
				}
				emitted = true
			}
			for _, tc := range ch.Delta.ToolCalls {
				t, ok := tools[tc.Index]
				if !ok {
					t = &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					tools[tc.Index] = t
					toolOrder = append(toolOrder, tc.Index)
					if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: tc.Index, ToolCall: &ir.ToolCall{ID: t.ID, Name: t.Name}}); err != nil {
						emitErr = err
						return
					}
					emitted = true
				} else {
					if t.ID == "" && tc.ID != "" {
						t.ID = tc.ID
					}
					if t.Name == "" && tc.Function.Name != "" {
						t.Name = tc.Function.Name
					}
				}
				if tc.Function.Arguments != "" {
					t.Arguments += tc.Function.Arguments
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: tc.Index, Delta: tc.Function.Arguments}); err != nil {
						emitErr = err
						return
					}
					emitted = true
				}
			}
		}
	})

	sc := bufio.NewScanner(streamBody)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	sawDone := false
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, emitted, uperr.New(uperr.ClassCanceled, 0, "qoder: stream canceled: %v", err)
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if isDoneLine(line) {
			sawDone = true
			break
		}
		if err := d.streamLineError(line); err != nil {
			return nil, emitted, err
		}
		delta := extractDelta(strings.TrimSpace(line[5:]))
		if delta.Usage != nil {
			usageObj = delta.Usage
		}
		if !delta.isEmpty() {
			acc.accept(delta)
			if emitErr != nil {
				return nil, emitted, emitErr
			}
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(streamBody) {
			return nil, emitted, uperr.New(uperr.ClassUpstreamTimeout, 0, "qoder: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, emitted, uperr.New(uperr.ClassCanceled, 0, "qoder: stream canceled: %v", ctx.Err())
		}
		return nil, emitted, uperr.New(uperr.ClassTransport, 0, "qoder: stream read: %v", err)
	}
	if !sawDone && ctx.Err() == nil {
		return nil, emitted, uperr.New(uperr.ClassTransport, 0, "qoder: stream ended without [DONE] marker (connection truncated?)")
	}
	acc.flush()
	if emitErr != nil {
		return nil, emitted, emitErr
	}

	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: created, Message: ir.Message{Role: ir.RoleAssistant}}
	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *tools[idx])
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.StopReason = mapStop(acc.finalFinishReason())
	if usageObj != nil {
		u := irUsageFromUsageObj(usageObj)
		out.Usage = u
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &u}); err != nil {
			return nil, emitted, err
		}
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: out.StopReason}); err != nil {
		return nil, emitted, err
	}
	return out, emitted, nil
}

// irUsageFromUsageObj maps the stream's usage frame onto the IR usage
// (BP-98 Qoder-7c): tokens, charged credits (CreditKnown gates the "no
// data" vs "0 spent" distinction), and the undiscounted strike-through
// price (CreditOriginal, gated the same way — a promotion's discount is
// invisible without it).
func irUsageFromUsageObj(uo *usage) ir.Usage {
	u := ir.Usage{
		InputTokens:  uo.PromptTokens,
		OutputTokens: uo.CompletionTokens,
		Credit:       uo.Credits,
		CreditKnown:  uo.Billable,
	}
	if uo.OriginalCredits > 0 {
		u.CreditOriginal = uo.OriginalCredits
		u.CreditOriginalKnown = true
	}
	if uo.PromptTokensDetails != nil {
		u.CachedInputTokens = uo.PromptTokensDetails.CachedTokens
		u.CacheKnown = true
	}
	if uo.CompletionTokensDetails != nil {
		u.ReasoningTokens = uo.CompletionTokensDetails.ReasoningTokens
	}
	return u
}

func mapStop(reason string) ir.StopReason {
	switch reason {
	case "length":
		return ir.StopLength
	case "tool_calls":
		return ir.StopToolUse
	default:
		return ir.StopEnd
	}
}

// queueWaitSeconds parses the upstream queue answer's retryAfterSeconds.
// Returns 0 when the body is not a queue signal; a missing/invalid field
// falls back to defaultQueueWait (conservative 30s, reference parity).
func queueWaitSeconds(body string) time.Duration {
	low := strings.ToLower(body)
	if !strings.Contains(body, busyCode) && !strings.Contains(low, "isqueued") && !strings.Contains(low, "queue") {
		return 0
	}
	var env struct {
		RetryAfterSeconds any `json:"retryAfterSeconds"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(body)), &env); err == nil {
		switch v := env.RetryAfterSeconds.(type) {
		case float64:
			if v >= 1 {
				return clampQueueWait(time.Duration(v) * time.Second)
			}
		case string:
			if n, err := strconv.Atoi(v); err == nil && n >= 1 {
				return clampQueueWait(time.Duration(n) * time.Second)
			}
		}
	}
	return defaultQueueWait
}

// clampQueueWait caps one round at queueWaitMax.
func clampQueueWait(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	if d > queueWaitMax {
		return queueWaitMax
	}
	return d
}

// extractJSONObject returns the first {...} span of a body that may embed JSON
// inside an error envelope string.
func extractJSONObject(body string) string {
	start := strings.Index(body, "{")
	if start < 0 {
		return ""
	}
	return body[start:]
}

// streamLineError surfaces in-stream gateway errors (statusCodeValue >= 400 or
// {"success":false} bodies), including the 10605 queue-full signal.
func (d *Driver) streamLineError(line string) error {
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	var env struct {
		StatusCodeValue int    `json:"statusCodeValue"`
		Body            string `json:"body"`
	}
	if json.Unmarshal([]byte(payload), &env) != nil {
		return nil
	}
	busy := strings.Contains(env.Body, busyCode) || strings.Contains(env.Body, `"code":"10605"`)
	if env.StatusCodeValue >= 400 || busy || strings.Contains(env.Body, `"success":false`) {
		status := env.StatusCodeValue
		if busy {
			return uperr.New(uperr.ClassSoftRate, 429, "qoder: gateway busy (10605): %s", clip(env.Body, 200))
		}
		return classifyQoder(status, env.Body)
	}
	return nil
}

// buildChatBody converts the IR request into the Qoder wire body.
func (d *Driver) buildChatBody(call driver.ChatCall, rt *runtime, cat *ModelCatalog, requestSetID string) ([]byte, string, string, bool, string, error) {
	payload, err := irPayload(call)
	if err != nil {
		return nil, "", "", false, "", uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", err)
	}
	display, qoderKey, err := cat.resolveModel(call.UpstreamModel)
	if err != nil {
		return nil, "", "", false, "", uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", err)
	}
	messages := extractMessages(payload["messages"])
	b := newRequestBody()
	nid := newUUID()
	b.RequestID = nid
	b.ChatRecordID = nid
	// The queue protocol recognises a resend as the SAME queued request only
	// when it carries the same request_set_id (BP-92). A fresh one each retry
	// puts the request at the back of the queue again, which is why a queued
	// binding used to keep failing.
	if requestSetID == "" {
		requestSetID = newUUID()
	}
	b.RequestSetID = requestSetID
	b.SessionID = newUUID()
	b.Stream = true
	rt.mu.Lock()
	ut := rt.userType
	rt.mu.Unlock()
	if ut == "" {
		ut = "personal_standard"
	}
	b.AliyunUserType = ut
	b.ModelConfig.Key = qoderKey
	b.ChatContext.Extra.ModelConfig.Key = qoderKey

	effort, effErr := resolveReasoningEffort(payload)
	if effErr != nil {
		return nil, "", "", false, "", uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", effErr)
	}
	// Snap the requested effort to the nearest level the model names
	// (BP-98 Qoder-9): an unsupported level passed through is ignored or
	// refused upstream, silently drifting from what the caller asked.
	if caps := cat.Caps[qoderKey]; caps != nil && len(caps.Efforts) > 0 && effort != "" && effort != "none" {
		effort = fitEffort(effort, caps.Efforts)
	}
	params, perr := buildParameters(payload, effort, cat.maxOutputTokens(qoderKey))
	if perr != nil {
		return nil, "", "", false, "", uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", perr)
	}
	b.Parameters = params
	reasoningOn := cat.reasoningDefault(qoderKey)
	if effort != "" {
		reasoningOn = effort != "none"
	}
	b.ModelConfig.IsReasoning = reasoningOn
	b.ChatContext.Extra.ModelConfig.IsReasoning = reasoningOn

	prompt := extractLatestUserPrompt(messages)
	b.ChatContext.Text.Text = prompt
	b.ChatContext.Extra.OriginalContent.Text = prompt
	// BP-111 task aggregation: the whole user turn (its tool loop included)
	// shares ONE (request_set_id, business.id, begin_at, stage) tuple, the way
	// the official client sends it. A fresh tuple per wire call made 50 tool
	// round-trips read upstream as 50 tasks; the aggregate counts task heads,
	// and "Billing daily count exceeded" arrives sooner the more of them a
	// session creates (magpie 0.2.31, live). request_id/chat_record_id stay
	// per-request — they name THIS wire call, and the queue protocol keys its
	// resends on request_set_id, which now carries the task identity too (a
	// queued resend of the same turn rejoining its own task is correct).
	seed := call.Request.SessionKey
	if seed == "" {
		seed = sessionSeedOf(messages)
	}
	b.SessionID = rt.sessions.sessionIDFor(seed)
	task, stage := rt.sessions.taskFor(turnFingerprint(seed, prompt), taskNameFor(prompt), time.Now())
	b.RequestSetID = task.id
	b.Business.ID = task.id
	b.Business.BeginAt = task.begin
	b.Business.Name = task.name
	b.Business.Stage = stage
	toolsEnabled := applyToolConfig(b, payload)
	b.Messages = buildQoderMessages(messages, prompt, toolsEnabled)
	markQoderCache(b.Messages)

	hasImages := false
	for _, m := range messages {
		if len(extractMessageImages(m)) > 0 {
			hasImages = true
			break
		}
	}
	if hasImages && !cat.VisionModels[display] {
		return nil, "", "", false, "", uperr.New(uperr.ClassBadRequest, 400,
			"qoder: image input is not supported by model %q", display)
	}
	if hasImages {
		b.ModelConfig.IsVL = true
		b.ChatContext.Extra.ModelConfig.IsVL = true
	}
	raw, err := marshalNoEscape(b)
	if err != nil {
		return nil, "", "", false, "", uperr.New(uperr.ClassInternal, 0, "qoder: marshal body: %v", err)
	}
	return raw, display, qoderKey, toolsEnabled, requestSetID, nil
}

// irPayload renders the IR request in the OpenAI shape the chat.go converters
// expect (messages array of interfaces, standard parameter keys).
func irPayload(call driver.ChatCall) (map[string]any, error) {
	req := call.Request
	msgs := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		wire := map[string]any{"role": string(m.Role)}
		switch {
		case m.Role == ir.RoleTool:
			wire["content"] = ir.TextOf(m.Content)
		case len(m.Content) == 1 && m.Content[0].Kind == "text":
			wire["content"] = m.Content[0].Text
		case len(m.Content) > 0:
			parts := make([]any, 0, len(m.Content))
			for _, p := range m.Content {
				switch p.Kind {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				case "image":
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:" + p.ImageMIME + ";base64," + p.ImageB64},
					})
				}
			}
			wire["content"] = parts
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
				})
			}
			wire["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		msgs = append(msgs, wire)
	}
	payload := map[string]any{"model": call.UpstreamModel, "messages": msgs, "stream": true}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.MaxTokens > 0 {
		// chat.go's maxTokensField expects JSON-decoded types (float64); a Go
		// int would fail with "max_tokens must be a number".
		payload["max_tokens"] = float64(req.MaxTokens)
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		payload["tools"] = tools
	}
	if req.ToolChoice != nil {
		payload["tool_choice"] = req.ToolChoice
	}
	if len(req.Stop) > 0 {
		payload["stop"] = req.Stop
	}
	if req.RequestedEffort != "" {
		payload["reasoning_effort"] = req.RequestedEffort
	}
	return payload, nil
}

// --- sessions ---

func (d *Driver) runtimeFor(acc *model.Account) (*runtime, error) {
	if acc == nil {
		return nil, fmt.Errorf("no account")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if rt := d.rt[acc.ID]; rt != nil {
		return rt, nil
	}
	c, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, err
	}
	changed := false
	if c.MachineID == "" || c.MachineToken == "" || c.MachineType == "" {
		// Derived device identity (2026-10-21): the machine code must be a
		// stable virtual device per ACCOUNT, not a random UUID. Random ids were
		// stable only while the stored credential lived, so every re-login
		// looked like a new device to upstream risk control. The seed is the
		// account's own identity plus this install's salt (see
		// deriveMachineIDs), so unrelated accounts never share a device and the
		// same account keeps one.
		seed := c.PAT
		if seed == "" {
			seed = acc.ID
		}
		m := deriveMachineIDs(patFingerprint(seed), d.machineSalt())
		c.MachineID, c.MachineToken, c.MachineType = m.machineID, m.machineToken, m.machineType
		changed = true
	}
	if changed {
		acc.CredsJSON = c.Marshal()
		acc.CredsDirty = true
	}
	rt := &runtime{creds: *c, sessions: newSessionTable()}
	d.rt[acc.ID] = rt
	return rt, nil
}

func (rt *runtime) needsRefreshLocked() bool {
	if rt.sess == nil {
		return true
	}
	if rt.expireTimeMs == 0 {
		return time.Since(rt.refreshedAt) > 30*time.Minute
	}
	// 30-minute lead (BP-98 Qoder-5): the renewal endpoint grants 2h, so the
	// old 2h lead made a refreshed session read as stale immediately and
	// every caller renewed again — the one-time refresh token spent per
	// request. 30min keeps the renewal ahead of expiry without eating the
	// whole grant.
	return time.Now().UnixMilli() > rt.expireTimeMs-30*60*1000
}

// ensureFresh serializes session renewal per runtime: the refresh token is
// one-time, so two renewals would spend it twice (magpie 0.2.30: "a refresh
// token is spent once, so two refreshes would spend it twice"). Renewals
// chain on renMu; a caller that acquires the lock after a completed renewal
// re-checks and finds the session fresh.
func ensureFresh(rt *runtime, fn func() error) error {
	rt.mu.Lock()
	for rt.renewing {
		// Another caller is renewing right now: wait for it and re-check.
		// A completed renewal makes the session fresh, so the waiter returns
		// nil without touching upstream (the refresh token is one-time —
		// racing it spends it twice, magpie 0.2.30 "a refresh token is spent
		// once, so two refreshes would spend it twice").
		rt.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		rt.mu.Lock()
		if rt.bootstrapped && !rt.needsRefreshLocked() {
			rt.mu.Unlock()
			return nil
		}
	}
	rt.renewing = true
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		rt.renewing = false
		rt.mu.Unlock()
	}()
	return fn()
}

func (d *Driver) ensureFreshSession(ctx context.Context, rt *runtime) error {
	return ensureFresh(rt, func() error {
		rt.mu.Lock()
		need := !rt.bootstrapped || rt.needsRefreshLocked()
		stale := need
		refresh := rt.refreshToken
		rt.mu.Unlock()
		if !need {
			return nil
		}
		// Renew first through the client's own endpoint (BP-98 Qoder-5,
		// magpie-community/plugins packages/qoder 0.2.30 refreshJob/renew):
		// a PAT exchange is a fresh sign-in and reads as one to upstream
		// risk control, while /api/v1/jobToken/refresh is the ordinary
		// renewal.
		if stale && rt.bootstrapped && refresh != "" {
			if err := d.refreshJobToken(ctx, rt, refresh); err == nil {
				return nil
			} else if ue, ok := uperr.As(err); ok && ue.Class != uperr.ClassAuth {
				// A transient refresh failure (network, 5xx): the PAT
				// exchange below is the fallback, not a reason to fail.
				log.Printf("qoder: job token refresh failed (%v); falling back to PAT exchange", err)
			}
			// An auth-class refresh refusal (lapsed refresh token) falls
			// through to the PAT exchange — the PAT lasts a year, the
			// refresh two days.
		}
		jt, err := d.exchangeJobToken(ctx, &rt.creds)
		if err != nil {
			return err
		}
		return d.applyJobToken(ctx, rt, jt)
	})
}

// refreshJobToken renews the job token pair through the openapi refresh
// endpoint (magpie 0.2.30 refreshJob): {token, refresh_token, expires_in},
// both halves required — a partial pair would orphan the next renewal. The
// renewed pair replaces the identity's tokens and the COSY session is
// rebuilt from it (the info blob embeds securityOauthToken/refreshToken).
func (d *Driver) refreshJobToken(ctx context.Context, rt *runtime, refreshToken string) error {
	body, _ := json.Marshal(map[string]any{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.openAPIBase()+pathJobRefresh, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder job token refresh: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return uperr.New(uperr.ClassAuth, resp.StatusCode, "qoder: refresh token lapsed: %s", clip(string(raw), 160))
	}
	if resp.StatusCode != http.StatusOK {
		return uperr.New(uperr.ClassTransport, resp.StatusCode, "qoder job token refresh: status %d: %s", resp.StatusCode, clip(string(raw), 160))
	}
	var out struct {
		Token     string `json:"token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("qoder job token refresh: decode: %w", err)
	}
	if out.Token == "" || out.Refresh == "" {
		return fmt.Errorf("qoder job token refresh: incomplete token pair")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.securityOauth = out.Token
	rt.refreshToken = out.Refresh
	if out.ExpiresIn > 0 {
		rt.expireTimeMs = time.Now().UnixMilli() + out.ExpiresIn*1000
	} else {
		rt.expireTimeMs = time.Now().Add(24 * time.Hour).UnixMilli()
	}
	rt.refreshedAt = time.Now()
	// Rebuild the COSY session from the renewed identity.
	sess, err := newSession(authIdentity{
		Name: rt.creds.Name, Aid: rt.uid, UID: rt.uid, UserType: rt.userType,
		SecurityOauthToken: out.Token, RefreshToken: out.Refresh,
	}, rt.creds.MachineID, rt.creds.MachineToken, rt.creds.MachineType)
	if err != nil {
		return err
	}
	rt.sess = sess
	return nil
}

func (d *Driver) applyJobToken(ctx context.Context, rt *runtime, jt map[string]any) error {
	name := toString(jt["name"])
	id := toString(jt["id"])
	ut := toString(jt["userType"])
	sec := toString(jt["securityOauthToken"])
	ref := toString(jt["refreshToken"])
	if ut == "" && id != "" {
		if u, err := d.fetchUserStatus(ctx, &rt.creds, id); err == nil {
			ut = u
		}
	}
	if ut == "" {
		rt.mu.Lock()
		ut = rt.userType
		rt.mu.Unlock()
	}
	if ut == "" {
		ut = "personal_standard"
	}
	sess, err := newSession(authIdentity{
		Name: name, Aid: id, UID: id, UserType: ut,
		SecurityOauthToken: sec, RefreshToken: ref,
	}, rt.creds.MachineID, rt.creds.MachineToken, rt.creds.MachineType)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.sess = sess
	rt.uid = id
	rt.userType = ut
	rt.securityOauth = sec
	rt.refreshToken = ref
	rt.expireTimeMs = toInt64(jt["expireTime"])
	rt.refreshedAt = time.Now()
	rt.bootstrapped = true
	rt.mu.Unlock()
	return nil
}

func (d *Driver) forceRefresh(ctx context.Context, rt *runtime) error {
	jt, err := d.exchangeJobToken(ctx, &rt.creds)
	if err != nil {
		return err
	}
	return d.applyJobToken(ctx, rt, jt)
}

// Refresh implements driver.Refresher (background refresh task).
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	rt, err := d.runtimeFor(acc)
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "qoder: %v", err)
	}
	rt.mu.Lock()
	rt.bootstrapped = false
	rt.mu.Unlock()
	return qoderError(d.ensureFreshSession(ctx, rt))
}

// --- signed HTTP ---

type jobTokenInner struct {
	PersonalToken      string          `json:"personalToken"`
	SecurityOauthToken string          `json:"securityOauthToken,omitempty"`
	RefreshToken       string          `json:"refreshToken,omitempty"`
	NeedRefresh        bool            `json:"needRefresh,omitempty"`
	AuthInfo           json.RawMessage `json:"authInfo"`
}

type encodedPayload struct {
	Payload       string `json:"payload"`
	EncodeVersion string `json:"encodeVersion"`
}

func qoderCommonHeaders(c *Creds, date, sig string) http.Header {
	h := http.Header{}
	h.Set("cosy-machinetoken", c.MachineToken)
	h.Set("cosy-machinetype", c.MachineType)
	h.Set("login-version", "v2")
	h.Set("appcode", appCode)
	h.Set("accept", "application/json")
	h.Set("accept-encoding", "identity")
	h.Set("cosy-version", "0.1.43")
	h.Set("cosy-clienttype", "5")
	h.Set("date", date)
	h.Set("signature", sig)
	h.Set("content-type", "application/json")
	h.Set("cosy-machineid", c.MachineID)
	h.Set("user-agent", "Go-http-client/2.0")
	return h
}

// bearerHeaders builds the COSY-signed chat/model-list request headers.
func bearerHeaders(sess *sessionContext, dateUnix int64, bearer, accept string) http.Header {
	h := http.Header{}
	h.Set("cosy-data-policy", "AGREE")
	h.Set("content-type", "application/json")
	h.Set("cosy-machinetype", sess.machineType)
	h.Set("cosy-clienttype", "5")
	h.Set("cosy-date", strconv.FormatInt(dateUnix, 10))
	h.Set("cosy-user", sess.ident.UID)
	h.Set("cosy-key", sess.cosyKey)
	h.Set("accept", accept)
	h.Set("authorization", bearer)
	h.Set("accept-encoding", "identity")
	h.Set("cosy-version", "0.1.43")
	h.Set("cosy-machineid", sess.machineID)
	h.Set("cosy-machinetoken", sess.machineToken)
	h.Set("login-version", "v2")
	h.Set("user-agent", "Go-http-client/2.0")
	return h
}

func (d *Driver) postEncoded(ctx context.Context, url string, obj any, c *Creds) (map[string]any, error) {
	date := signDate()
	sig := signDateMD5(date)
	plain, err := marshalNoEscape(obj)
	if err != nil {
		return nil, err
	}
	body, err := gatewayEncode(plain)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = qoderCommonHeaders(c, date, sig)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, classifyQoder(resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, uperr.New(uperr.ClassTransport, resp.StatusCode, "qoder: decode response: %v", err)
	}
	return out, nil
}

func (d *Driver) exchangeJobToken(ctx context.Context, c *Creds) (map[string]any, error) {
	inner, err := marshalNoEscape(jobTokenInner{PersonalToken: c.PAT, AuthInfo: json.RawMessage("{}")})
	if err != nil {
		return nil, err
	}
	return d.postEncoded(ctx, d.gatewayBase()+jobPath, encodedPayload{Payload: string(inner), EncodeVersion: "1"}, c)
}

func (d *Driver) fetchUserStatus(ctx context.Context, c *Creds, userID string) (string, error) {
	inner, err := marshalNoEscape(struct {
		UserID             string          `json:"userId"`
		PersonalToken      string          `json:"personalToken"`
		SecurityOauthToken string          `json:"securityOauthToken,omitempty"`
		RefreshToken       string          `json:"refreshToken,omitempty"`
		NeedRefresh        bool            `json:"needRefresh,omitempty"`
		AuthInfo           json.RawMessage `json:"authInfo"`
	}{UserID: userID, AuthInfo: json.RawMessage("{}")})
	if err != nil {
		return "", err
	}
	raw, err := d.postEncoded(ctx, d.gatewayBase()+statPath, encodedPayload{Payload: string(inner), EncodeVersion: "1"}, c)
	if err != nil {
		return "", err
	}
	return toString(raw["userType"]), nil
}

func (d *Driver) fetchModelCatalog(ctx context.Context, sess *sessionContext) (map[string]any, error) {
	full := d.gatewayBase() + modelPath
	dateUnix := time.Now().Unix()
	bearer := sess.buildBearer(dateUnix, "", sigPath(full))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header = bearerHeaders(sess, dateUnix, bearer, "application/json")
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, classifyQoder(resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	return out, nil
}

// --- quota / checkin ---

func (d *Driver) openBearer(ctx context.Context, rt *runtime) (string, error) {
	rt.mu.Lock()
	tok, exp := rt.bearerTok, rt.bearerExpMs
	rt.mu.Unlock()
	if tok != "" && (exp == 0 || time.Now().UnixMilli() < exp-10*60*1000) {
		return tok, nil
	}
	body, _ := marshalNoEscape(map[string]string{"personal_token": rt.creds.PAT})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.openAPIBase()+pathJobExchange, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := d.rpc.Do(req)
	if err != nil {
		return "", uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", classifyQoder(resp.StatusCode, string(raw))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("exchange returned no token: %s", clip(string(raw), 120))
	}
	rt.mu.Lock()
	rt.bearerTok = out.Token
	if out.ExpiresIn > 0 {
		rt.bearerExpMs = time.Now().UnixMilli() + out.ExpiresIn
	}
	rt.mu.Unlock()
	return out.Token, nil
}

func deriveCheckinID(uid, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])
}

// checkinHeaders builds the campaigns/check-in header set. machineToken and
// machineType come from the account's derived device triple (BP-98 Qoder-2).
func checkinHeaders(bearer, uid, machineToken, machineType string) map[string]string {
	h := map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json, text/plain, */*",
		"User-Agent":    campaignUserAgent,
		"Authorization": "Bearer " + bearer,
		"X-Request-ID":  deriveCheckinID(uid, "req") + fmt.Sprintf("-%06d", time.Now().UnixNano()%1e6),
		"X-Machine-ID":  deriveCheckinID(uid, "machine"),
		"X-Session-ID":  deriveCheckinID(uid, "session"),
		"Origin":        "https://qoder.com.cn",
		"Referer":       "https://qoder.com.cn/",
		// Device identity on the Cosy header family (BP-98 Qoder-2,
		// magpie-community/plugins packages/qoder 0.2.30 campaignCall):
		// Qoder CN moved the daily 100 credits to the desktop benefit center
		// and the campaigns list omits the CLAIM_BENEFIT entry unless the
		// request carries the account's device identity.
		"Cosy-ClientType": "10",
	}
	if machineToken != "" {
		h["Cosy-MachineToken"] = machineToken
	}
	if machineType != "" {
		h["Cosy-MachineType"] = machineType
	}
	return h
}

// campaignUserAgent reads as the client, not as a Go script (magpie 0.2.30
// campaignCall sends "Qoder"; a bare UA on the campaigns page reads as
// tooling).
const campaignUserAgent = "Qoder"

// campaignPath is where Qoder lists the account's campaigns — the daily
// credits among them (actionType CLAIM_BENEFIT) — and claims one
// (magpie 0.2.30 CAMPAIGNS).
const (
	campaignPath  = "/sash/api/v1/me/campaigns"
	campaignClaim = "/sash/api/v1/me/campaigns/%s/claim"
)

func (d *Driver) checkinJSON(ctx context.Context, method, url string, body []byte, uid, bearer string) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	mt, mty := d.machineIdentityFor(uid)
	for k, v := range checkinHeaders(bearer, uid, mt, mty) {
		req.Header.Set(k, v)
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusConflict {
		return map[string]any{"result": "ALREADY_CLAIMED"}, nil
	}
	if resp.StatusCode >= 400 {
		return nil, classifyQoder(resp.StatusCode, string(raw))
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", url, err)
		}
	}
	return out, nil
}

// machineIdentityFor returns the Cosy device-identity pair for the account's
// derived virtual device (the same triple the chat calls sign with), or two
// empty strings when no salt is configured — the headers are then omitted
// and the campaigns list may omit the daily entry (BP-98 Qoder-2).
func (d *Driver) machineIdentityFor(uid string) (token, typ string) {
	m := deriveMachineIDs(patFingerprint(uid), d.machineSalt())
	return m.machineToken, m.machineType
}

// Checkin implements the daily check-in task (idempotent upstream).
//
// Two paths (BP-98 Qoder-2): the legacy daily-check-in endpoints first; when
// they stop describing the daily credit (Qoder CN moved it to the desktop
// benefit center — the 0.2.30 clients read it from the campaigns page), the
// campaigns list claims the CLAIM_BENEFIT campaign instead. A campaign
// already claimed is the day's done, one not active is inactive — never an
// error worth retrying.
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) (checkedIn bool, credits int64, enable bool, err error) {
	rt, err := d.runtimeFor(acc)
	if err != nil {
		return false, 0, false, uperr.New(uperr.ClassAuth, 0, "qoder: %v", err)
	}
	bearer, err := d.openBearer(ctx, rt)
	if err != nil {
		return false, 0, false, qoderError(err)
	}
	rt.mu.Lock()
	uid := rt.uid
	rt.mu.Unlock()
	out, err := d.checkinJSON(ctx, http.MethodGet, d.openAPIBase()+pathCheckStatus, nil, uid, bearer)
	if err != nil {
		return false, 0, false, qoderError(err)
	}
	switch status := toString(out["status"]); status {
	case "CLAIMED":
		return true, 0, true, nil
	case "DISABLED":
		// DISABLED is NOT "this account has no check-in programme" (BP-120).
		// Qoder CN retired the legacy daily-check-in endpoint and moved the
		// daily 100 credits into the desktop benefit centre, where the legacy
		// status endpoint answers DISABLED for everyone. Returning enable=false
		// here made the scheduler report "qoder checkin not available" every
		// day and never look at the campaigns page — which, read live on
		// 2026-10-26, carries an ACTIVE CLAIM_BENEFIT campaign
		// ("每天领 100 Credits", 每日 10:00 UTC+8 刷新, 30 天有效) whose
		// claimStatus was CLAIMED, i.e. the account HAD been claiming — just
		// never through us. Fall through to the campaigns path; it is the only
		// path that can still answer "claimable" on today's upstream.
		return d.checkinCampaigns(ctx, uid, bearer)
	case "CLAIMABLE":
		claim, cerr := d.checkinJSON(ctx, http.MethodPost, d.openAPIBase()+pathCheckClaim, []byte("{}"), uid, bearer)
		if cerr != nil {
			return false, 0, false, qoderError(cerr)
		}
		if toString(claim["result"]) == "ALREADY_CLAIMED" {
			return true, 0, true, nil
		}
		if succ, ok := claim["success"].(bool); ok && !succ && claim["result"] == nil {
			return false, 0, false, fmt.Errorf("qoder checkin rejected: %s", clip(marshalCompact(claim), 160))
		}
		reward := toInt64(claim["rewardCredits"])
		// Pro upgrade pack is one-off; claim it best-effort after a fresh sign-in.
		if elig, eerr := d.checkinJSON(ctx, http.MethodGet, d.openAPIBase()+pathProElig, nil, uid, bearer); eerr == nil {
			if ok, _ := elig["eligible"].(bool); ok {
				_, _ = d.checkinJSON(ctx, http.MethodPost, d.openAPIBase()+pathProClaim, []byte("{}"), uid, bearer)
			}
		}
		return false, reward, true, nil
	default:
		// The legacy endpoint no longer describes the daily credit: read the
		// campaigns page the current client uses.
		return d.checkinCampaigns(ctx, uid, bearer)
	}
}

// checkinCampaigns claims the daily credits through the campaigns page the
// current Qoder client uses (magpie-community/plugins packages/qoder 0.2.30
// checkin): list /sash/api/v1/me/campaigns, find the active CLAIM_BENEFIT
// campaign, claim it. CLAIMED = the day's done; no active campaign =
// inactive (not an error).
func (d *Driver) checkinCampaigns(ctx context.Context, uid, bearer string) (bool, int64, bool, error) {
	list, err := d.checkinJSON(ctx, http.MethodGet, d.openAPIBase()+campaignPath, nil, uid, bearer)
	if err != nil {
		return false, 0, false, qoderError(err)
	}
	rawList, _ := list["campaigns"].([]any)
	now := time.Now().Unix()
	var claimable map[string]any
	for _, it := range rawList {
		c, ok := it.(map[string]any)
		if !ok || toString(c["actionType"]) != "CLAIM_BENEFIT" {
			continue
		}
		start := toInt64(c["startAt"])
		end := toInt64(c["endAt"])
		active := (start == 0 || start <= now) && (end == 0 || end > now)
		switch toString(c["claimStatus"]) {
		case "CLAIMABLE":
			if active {
				claimable = c
			}
		case "CLAIMED":
			if active {
				return true, toInt64(c["benefit.amount"]), true, nil
			}
		}
	}
	if claimable == nil {
		return false, 0, true, nil // no active campaign: inactive, not an error
	}
	id := toString(claimable["id"])
	if id == "" {
		return false, 0, true, fmt.Errorf("qoder campaigns: CLAIM_BENEFIT campaign without id")
	}
	claim, err := d.checkinJSON(ctx, http.MethodPost, d.openAPIBase()+fmt.Sprintf(campaignClaim, id), []byte("{}"), uid, bearer)
	if err != nil {
		return false, 0, false, qoderError(err)
	}
	if toString(claim["result"]) == "ALREADY_CLAIMED" {
		return true, 0, true, nil
	}
	if succ, ok := claim["success"].(bool); ok && !succ {
		return false, 0, false, fmt.Errorf("qoder campaigns claim rejected: %s", clip(marshalCompact(claim), 160))
	}
	reward := toInt64(claim["rewardCredits"])
	if reward == 0 {
		reward = toInt64(claimable["benefit.amount"])
	}
	// A fresh campaigns claim IS the day's check-in done: the task ledger
	// must record it as ok/checked-in so the next slot does not re-run it
	// (the legacy path returned false here because its status endpoint
	// reports CLAIMED the next time; the campaigns page has no such
	// re-read in the same day).
	return true, reward, true, nil
}

// CatalogMap returns the live display-name -> upstream-key catalogue
// (diagnostics; the static fallback is not merged in).
func (d *Driver) CatalogMap(ctx context.Context, acc *model.Account) (map[string]string, error) {
	rt, err := d.runtimeFor(acc)
	if err != nil {
		return nil, err
	}
	if err := d.ensureFreshSession(ctx, rt); err != nil {
		return nil, qoderError(err)
	}
	rt.mu.Lock()
	sess := rt.sess
	rt.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("no session")
	}
	raw, err := d.fetchModelCatalog(ctx, sess)
	if err != nil {
		return nil, qoderError(err)
	}
	cat := extractCatalog(raw)
	if cat == nil {
		return nil, fmt.Errorf("live catalogue is empty")
	}
	out := map[string]string{}
	for k, v := range cat.ModelMap {
		out[k] = v
	}
	return out, nil
}

// QuotaRaw fetches the raw quota document (diagnostics / future panel detail).
func (d *Driver) QuotaRaw(ctx context.Context, acc *model.Account) (map[string]any, error) {
	rt, err := d.runtimeFor(acc)
	if err != nil {
		return nil, err
	}
	if err := d.ensureFreshSession(ctx, rt); err != nil {
		return nil, qoderError(err)
	}
	rt.mu.Lock()
	tok := rt.securityOauth
	rt.mu.Unlock()
	if tok == "" {
		if tok, err = d.openBearer(ctx, rt); err != nil {
			return nil, qoderError(err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.openAPIBase()+quotaPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyQoder(resp.StatusCode, string(raw))
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "qoder: quota decode: %v", err)
	}
	return doc, nil
}

// Probe implements driver.Prober via the OpenAPI quota endpoint.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	rt, err := d.runtimeFor(acc)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "qoder: %v", err)
	}
	if err := d.ensureFreshSession(ctx, rt); err != nil {
		return nil, qoderError(err)
	}
	rt.mu.Lock()
	tok := rt.securityOauth
	rt.mu.Unlock()
	if tok == "" {
		tok, err = d.openBearer(ctx, rt)
		if err != nil {
			return nil, qoderError(err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.openAPIBase()+quotaPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyQoder(resp.StatusCode, string(raw))
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "qoder: quota decode: %v", err)
	}
	return probeCreditsFromDoc(doc), nil
}

// probeCreditsFromDoc maps the quota document onto the balance: plan remain/
// total, addon + org + dedicated packages as Bonus (BP-98 Qoder-7b — the
// dedicatedResourcePackages array used to be dropped, so an account with a
// dedicated pack read as poorer than it is; magpie 0.2.30 parseUsage adds
// every package under "Dedicated credits").
func probeCreditsFromDoc(doc map[string]any) *driver.ProbeResult {
	remain := toInt64(doc["plan_remaining"])
	total := toInt64(doc["plan_total"])
	addon := toInt64(doc["addon_remaining"])
	addonTotal := int64(0)
	note := fmt.Sprintf("plan %d/%d, addon %d, org %d", toInt64(doc["plan_used"]), total,
		addon, toInt64(doc["org_remaining"]))
	// Live documents nest the numbers under userQuota (usageType=credits).
	if uq, ok := doc["userQuota"].(map[string]any); ok {
		remain = toInt64(uq["remaining"])
		total = toInt64(uq["total"])
		note = fmt.Sprintf("%s %d/%d %s (used %d)", toString(doc["userType"]),
			remain, total, toString(uq["unit"]), toInt64(uq["used"]))
		if aq, ok := doc["addOnQuota"].(map[string]any); ok {
			addon = toInt64(aq["remaining"])
			addonTotal = toInt64(aq["total"])
		}
	}
	bonus := addon
	bonusTotal := addonTotal
	if oq, ok := doc["orgResourcePackage"].(map[string]any); ok {
		if avail, _ := oq["available"].(bool); avail {
			bonus += toInt64(oq["remaining"])
			bonusTotal += toInt64(oq["total"])
		}
	}
	dedicated := int64(0)
	dedicatedTotal := int64(0)
	if pkgs, ok := doc["dedicatedResourcePackages"].([]any); ok {
		for _, it := range pkgs {
			if pkg, ok := it.(map[string]any); ok {
				dedicated += toInt64(pkg["remaining"])
				dedicatedTotal += toInt64(pkg["total"])
			}
		}
	}
	if dedicated > 0 {
		bonus += dedicated
		bonusTotal += dedicatedTotal
		note += fmt.Sprintf(", dedicated %d", dedicated)
	}
	return &driver.ProbeResult{Credits: &model.Credits{
		Remain: remain, Total: total, Bonus: bonus, BonusTotal: bonusTotal, UpdatedAt: time.Now(),
	}, Note: note}
}

// LoginPAT validates a personal access token and builds the account
// (panel onboarding). The stable identity is the jobToken id.
func (d *Driver) LoginPAT(ctx context.Context, pat, label string) (*model.Account, error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, "pt-") {
		return nil, fmt.Errorf("PAT 必须以 pt- 开头")
	}
	// The PAT's own identity seeds the device: onboarding the same PAT twice
	// yields the same virtual device instead of a fresh random one.
	m := deriveMachineIDs(patFingerprint(pat), d.machineSalt())
	c := &Creds{PAT: pat, Name: label, MachineID: m.machineID, MachineToken: m.machineToken, MachineType: m.machineType}
	jt, err := d.exchangeJobToken(ctx, c)
	if err != nil {
		return nil, err
	}
	id := toString(jt["id"])
	name := toString(jt["name"])
	if id == "" {
		return nil, fmt.Errorf("jobToken 响应缺少 id，无法建立稳定身份")
	}
	return &model.Account{
		ID: model.ChannelQoder + ":" + id, Channel: model.ChannelQoder,
		UID: id, Nickname: firstNonEmpty(label, name, "qoder"),
		CredsJSON: c.Marshal(),
	}, nil
}

// --- helpers ---

// quotaCodes are the codes Qoder's own client counts as a plan's usage or
// credits used up (magpie-community/plugins packages/qoder 0.2.30
// QUOTA_CODES, read from the app's qoder-agent-sdk worker): a 429-class
// exhaustion — without this split they landed in bad_request and the pool
// kept hammering an empty account instead of cooling it to the daily reset.
var quotaCodes = map[string]bool{
	"110": true, "112": true, "113": true, "114": true, "115": true,
	"116": true, "117": true, "118": true, "119": true, "122": true,
}

// requestCodes refuse THIS request, not the account: sensitive content, not
// supported, input too long, too many media, a custom model's service
// (magpie 0.2.30 REQUEST_CODES). bad_request stays right for these.
var requestCodes = map[string]bool{
	"406": true, "416": true, "430": true, "80411": true, "80412": true,
	"100400": true, "100401": true, "100403": true,
}

// upstreamFailedRe is Qoder saying the model's own vendor behind it failed:
// "Error in upstream response", which Qoder sends as a 400 (magpie 0.2.30
// UPSTREAM_FAILED, SlouchyQuill507 on Discord). Nothing is wrong with the
// request or the account — it is a transport failure the pool may retry
// elsewhere, not a bad request that ends the turn as the client's fault.
var upstreamFailedRe = regexp.MustCompile(`(?i)\berror in upstream response\b`)

// qoderBodyCode extracts the top-level business code from a Qoder body
// (number or digit string).
func qoderBodyCode(body string) string {
	var env struct {
		Code any `json:"code"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	switch t := env.Code.(type) {
	case float64:
		return fmt.Sprint(int(t))
	case string:
		return t
	}
	return ""
}

func classifyQoder(status int, body string) *uperr.Error {
	text := clip(body, 300)
	if strings.Contains(body, busyCode) || status == http.StatusTooManyRequests {
		return uperr.New(uperr.ClassSoftRate, status, "qoder: %s", text)
	}
	// The business code inside the body decides before the HTTP status does.
	code := qoderBodyCode(body)
	switch {
	case quotaCodes[code]:
		e := uperr.New(uperr.ClassHardCredit, 429, "qoder: usage limit reached (%s): %s", code, text)
		e.Code = code
		return e
	case upstreamFailedRe.MatchString(body):
		e := uperr.New(uperr.ClassTransport, 502, "qoder: upstream vendor failed: %s", text)
		if code != "" {
			e.Code = code
		}
		return e
	case requestCodes[code]:
		e := uperr.New(uperr.ClassBadRequest, 400, "qoder: refused the request (%s): %s", code, text)
		e.Code = code
		return e
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return uperr.New(uperr.ClassAuth, status, "qoder: %s", text)
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "qoder: %s", text)
	case status >= 400:
		return uperr.New(uperr.ClassBadRequest, status, "qoder: %s", text)
	default:
		return uperr.New(uperr.ClassInternal, status, "qoder: %s", text)
	}
}

// qoderError maps a non-uperr transport/context error into a class.
func qoderError(err error) error {
	if err == nil {
		return nil
	}
	var ue *uperr.Error
	if errors.As(err, &ue) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return uperr.New(uperr.ClassCanceled, 0, "qoder: %v", err)
	}
	return uperr.New(uperr.ClassTransport, 0, "qoder: %v", err)
}

func isDoneLine(line string) bool {
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	payload := strings.TrimSpace(line[5:])
	if payload == "[DONE]" {
		return true
	}
	var env struct {
		Body string `json:"body"`
	}
	if json.Unmarshal([]byte(payload), &env) == nil && env.Body == "[DONE]" {
		return true
	}
	return false
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// machineIDs is one derived virtual device (2026-10-21; qoder2api-hub /
// Zhengyuuuui Go port parity). The formulas are byte-identical to the
// reference's qoder_fingerprint.py for the same seed:
//
//	machineID     md5("machine:{seed}")              → 32 hex
//	machineType   md5("machinetype:{seed}")[:18]     → 18 hex
//	machineToken  sha512("machinetoken:{seed}")[:43] → 43 base64url
//
// Deriving from the account UID (+ an optional install salt) instead of
// minting random UUIDs gives one stable virtual device per account — the
// upstream never sees machine-code drift across re-logins — and unrelated
// accounts never share a device. Changing the salt changes every fingerprint
// at once, which is equivalent to moving the whole install to a new machine:
// generate it once, then never change it.
type machineIDs struct {
	machineID    string
	machineToken string
	machineType  string
}

func deriveMachineIDs(uid, salt string) machineIDs {
	if uid == "" {
		panic("qoder: deriveMachineIDs requires a non-empty seed (uid)")
	}
	seed := uid
	if salt != "" {
		seed = seed + "|salt:" + salt
	}
	id := md5Hex("machine:" + seed)
	typ := md5Hex("machinetype:" + seed)[:18]
	sum := sha512.Sum512([]byte("machinetoken:" + seed))
	tok := base64.RawURLEncoding.EncodeToString(sum[:])[:43]
	return machineIDs{machineID: id, machineToken: tok, machineType: typ}
}

// machineSalt returns this install's device-fingerprint salt, persisted under
// the data directory so it survives restarts. It must be generated once and
// then never change: editing it rotates every account's virtual device, which
// upstream reads as "the same user suddenly appeared on a new machine".
//
// A fresh random salt per process would be worse than useless — every restart
// would mint new device credentials for accounts that need them regenerated,
// so the salt is written to disk on first use. Empty dir (tests, library use)
// falls back to a stable in-process constant.
func (d *Driver) machineSalt() string {
	d.saltOnce.Do(func() {
		if d.cfg.DataDir == "" {
			d.saltVal = "qoder-default-salt"
			return
		}
		path := filepath.Join(d.cfg.DataDir, "qoder-device-salt")
		if raw, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(raw)); s != "" {
				d.saltVal = s
				return
			}
		}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			d.saltVal = "qoder-fallback-salt"
			return
		}
		s := hex.EncodeToString(b)
		if err := os.WriteFile(path, []byte(s+"\n"), 0o600); err != nil {
			log.Printf("qoder: could not persist device salt (%v); using in-memory value", err)
		}
		d.saltVal = s
	})
	return d.saltVal
}

// patFingerprint derives the seed for a PAT-based onboarding: the same PAT
// always maps to the same virtual device, without storing the PAT itself in
// the seed.
func patFingerprint(pat string) string {
	sum := sha512.Sum512([]byte("pat:" + strings.TrimSpace(pat)))
	return hex.EncodeToString(sum[:])[:32]
}

func newMachineIDs() (id, token, typ string) {
	return newUUID(), newUUID() + newUUID(), newUUID()[:18]
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func marshalCompact(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncateRunes(string(raw), 160)
}

// randIntN returns a pseudo-random int in [0,n) for queue-wait jitter. The
// jitter desynchronises clients that were queued on the same model, so they do
// not wake up and stampede the queue together.
func randIntN(n int) int {
	if n <= 0 {
		return 0
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	v := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if v < 0 {
		v = -v
	}
	return v % n
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
