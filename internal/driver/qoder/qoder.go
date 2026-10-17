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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

	pathJobExchange = "/api/v1/jobToken/exchange"
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
	creds         Creds
	uid           string
	userType      string
	securityOauth string
	refreshToken  string
	expireTimeMs  int64
	refreshedAt   time.Time
	bootstrapped  bool
	sess          *sessionContext

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
	for attempt := 0; attempt < 2; attempt++ {
		if err := d.ensureFreshSession(ctx, rt); err != nil {
			return nil, qoderError(err)
		}
		res, emitted, err := d.streamOnce(ctx, rt, call, emit)
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
		case ue.Class == uperr.ClassSoftRate && attempt == 0 && strings.Contains(ue.Message, busyCode):
			select {
			case <-ctx.Done():
				return nil, qoderError(ctx.Err())
			case <-time.After(2 * time.Second):
			}
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (d *Driver) streamOnce(ctx context.Context, rt *runtime, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, bool, error) {
	cat := d.getCatalog(ctx, rt)
	body, display, qoderKey, toolsEnabled, err := d.buildChatBody(call, rt, cat)
	if err != nil {
		return nil, false, err
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
		u := ir.Usage{
			InputTokens:  usageObj.PromptTokens,
			OutputTokens: usageObj.CompletionTokens,
			Credit:       usageObj.Credits,
			CreditKnown:  usageObj.Billable,
		}
		if usageObj.PromptTokensDetails != nil {
			u.CachedInputTokens = usageObj.PromptTokensDetails.CachedTokens
			u.CacheKnown = true
		}
		if usageObj.CompletionTokensDetails != nil {
			u.ReasoningTokens = usageObj.CompletionTokensDetails.ReasoningTokens
		}
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
func (d *Driver) buildChatBody(call driver.ChatCall, rt *runtime, cat *ModelCatalog) ([]byte, string, string, bool, error) {
	payload, err := irPayload(call)
	if err != nil {
		return nil, "", "", false, uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", err)
	}
	display, qoderKey, err := cat.resolveModel(call.UpstreamModel)
	if err != nil {
		return nil, "", "", false, uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", err)
	}
	messages := extractMessages(payload["messages"])
	b := newRequestBody()
	nid := newUUID()
	b.RequestID = nid
	b.ChatRecordID = nid
	b.RequestSetID = newUUID()
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
		return nil, "", "", false, uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", effErr)
	}
	params, perr := buildParameters(payload, effort, cat.maxOutputTokens(qoderKey))
	if perr != nil {
		return nil, "", "", false, uperr.New(uperr.ClassBadRequest, 400, "qoder: %v", perr)
	}
	b.Parameters = params
	reasoningOn := cat.reasoningDefault(qoderKey)
	if effort != "" {
		reasoningOn = effort != "none"
	}
	b.ModelConfig.IsReasoning = reasoningOn
	b.ChatContext.Extra.ModelConfig.IsReasoning = reasoningOn
	b.Business.ID = newUUID()
	b.Business.BeginAt = time.Now().UnixMilli()

	prompt := extractLatestUserPrompt(messages)
	b.ChatContext.Text.Text = prompt
	b.ChatContext.Extra.OriginalContent.Text = prompt
	b.Business.Name = truncateRunes(prompt, 30)
	toolsEnabled := applyToolConfig(b, payload)
	b.Messages = buildQoderMessages(messages, prompt, toolsEnabled)

	hasImages := false
	for _, m := range messages {
		if len(extractMessageImages(m)) > 0 {
			hasImages = true
			break
		}
	}
	if hasImages && !cat.VisionModels[display] {
		return nil, "", "", false, uperr.New(uperr.ClassBadRequest, 400,
			"qoder: image input is not supported by model %q", display)
	}
	if hasImages {
		b.ModelConfig.IsVL = true
		b.ChatContext.Extra.ModelConfig.IsVL = true
	}
	raw, err := marshalNoEscape(b)
	if err != nil {
		return nil, "", "", false, uperr.New(uperr.ClassInternal, 0, "qoder: marshal body: %v", err)
	}
	return raw, display, qoderKey, toolsEnabled, nil
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
		c.MachineID, c.MachineToken, c.MachineType = newMachineIDs()
		changed = true
	}
	if changed {
		acc.CredsJSON = c.Marshal()
		acc.CredsDirty = true
	}
	rt := &runtime{creds: *c}
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
	return time.Now().UnixMilli() > rt.expireTimeMs-2*3600*1000
}

func (d *Driver) ensureFreshSession(ctx context.Context, rt *runtime) error {
	rt.mu.Lock()
	need := !rt.bootstrapped || rt.needsRefreshLocked()
	rt.mu.Unlock()
	if !need {
		return nil
	}
	jt, err := d.exchangeJobToken(ctx, &rt.creds)
	if err != nil {
		return err
	}
	return d.applyJobToken(ctx, rt, jt)
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

func checkinHeaders(bearer, uid string) map[string]string {
	return map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json, text/plain, */*",
		"User-Agent":    "Go-http-client/2.0",
		"Authorization": "Bearer " + bearer,
		"X-Request-ID":  deriveCheckinID(uid, "req") + fmt.Sprintf("-%06d", time.Now().UnixNano()%1e6),
		"X-Machine-ID":  deriveCheckinID(uid, "machine"),
		"X-Session-ID":  deriveCheckinID(uid, "session"),
		"Origin":        "https://qoder.com.cn",
		"Referer":       "https://qoder.com.cn/",
	}
}

func (d *Driver) checkinJSON(ctx context.Context, method, url string, body []byte, uid, bearer string) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range checkinHeaders(bearer, uid) {
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

// Checkin implements the daily check-in task (idempotent upstream).
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
		return false, 0, false, nil
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
		return false, 0, true, nil
	}
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
	remain := toInt64(doc["plan_remaining"])
	total := toInt64(doc["plan_total"])
	addon := toInt64(doc["addon_remaining"])
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
		}
	}
	bonus := addon
	if oq, ok := doc["orgResourcePackage"].(map[string]any); ok {
		if avail, _ := oq["available"].(bool); avail {
			bonus += toInt64(oq["remaining"])
		}
	}
	return &driver.ProbeResult{Credits: &model.Credits{
		Remain: remain, Total: total, Bonus: bonus, UpdatedAt: time.Now(),
	}, Note: note}, nil
}

// LoginPAT validates a personal access token and builds the account
// (panel onboarding). The stable identity is the jobToken id.
func (d *Driver) LoginPAT(ctx context.Context, pat, label string) (*model.Account, error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, "pt-") {
		return nil, fmt.Errorf("PAT 必须以 pt- 开头")
	}
	mid, mtok, mtype := newMachineIDs()
	c := &Creds{PAT: pat, Name: label, MachineID: mid, MachineToken: mtok, MachineType: mtype}
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

func classifyQoder(status int, body string) *uperr.Error {
	text := clip(body, 300)
	if strings.Contains(body, busyCode) || status == http.StatusTooManyRequests {
		return uperr.New(uperr.ClassSoftRate, status, "qoder: %s", text)
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
