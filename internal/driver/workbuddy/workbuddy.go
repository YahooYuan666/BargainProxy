// Package workbuddy implements the Tencent CodeBuddy (workbuddy) channel:
// OpenAI-variant chat completions with forced streaming, device-flow login,
// credit balance queries with expiry buckets, daily checkin, growth tasks and
// the panel's error taxonomy (6004 model-scoped limit, 402 credit exhaustion,
// 12153 session death...).
//
// Endpoint behavior notes come from the clean-room research recorded in
// research/2026-10-13-三项目与生态调研.md. No code was copied from the GPL
// project; MIT reference code was re-implemented against the IR contract.
package workbuddy

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/prompt"
	"bargainproxy/internal/uperr"
)

// Creds is the persisted credential shape for a workbuddy account.
type Creds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix seconds
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Realm        string `json:"realm,omitempty"`        // "cn" | "global"
	DeviceToken  string `json:"device_token,omitempty"` // X-Device-Token when available
}

// RealmOrDefault normalizes the realm.
func (c *Creds) RealmOrDefault() string {
	if c.Realm == "global" {
		return "global"
	}
	return "cn"
}

// business codes observed in upstream bodies.
const (
	codeModelRate   = 6004  // model-scoped rate limit, body carries reset wall-clock
	codeBadParams   = 11101 // invalid params: do not penalize the account
	codeSessionDead = 12153 // session dead: needs re-login
	codeQueueFull   = 6021  // inference cluster queue full ("queue.full.title"): upstream capacity event, NOT an account problem — rotating accounts at request pace miskilled a whole pool in seconds in the field (2026-10-15); the right response is wait, not switch
	// codePromptTooLong: the prompt exceeds the model's context window. The
	// same body fails on every account, so this is request-level: no rotation,
	// no account penalty (2026-10-19).
	codePromptTooLong = 11115
	// codeAccountRestricted: upstream restricted the ACCOUNT ("request
	// illegal" + displayMsg "账号存在异常行为，已被限制使用"). Refreshing cannot
	// fix it; the account must leave routing. The same code also carries a
	// model-level rate-limit message, which must stay soft_rate — so the
	// wording decides, never the code alone (2026-10-20).
	codeAccountRestricted = 11140
	// codeChannelRefused: the channel check refused the caller's system
	// prompt as an "unapproved channel". Request-level, not the account's
	// fault; the driver retries once with the prompt moved into the user turn
	// (BP-93).
	codeChannelRefused = 11128
)

// Driver implements driver.Driver for the workbuddy channel.
type Driver struct {
	cfg  config.WorkBuddySettings
	http *http.Client // streaming chat calls (no total timeout + header/idle watchdogs)
	rpc  *http.Client // short billing / growth / refresh calls (total timeout)
	// promptReplace is the hot-switchable system-prompt replacement (panel
	// setting; default on). Off falls back to StripSystem like passthrough.
	promptReplace atomic.Bool
	refreshMu     sync.Mutex
	refreshes     map[string]*refreshFlight
}

// New builds the driver.
func New(cfg config.WorkBuddySettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	d := &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient()}
	d.promptReplace.Store(true)
	return d
}

// SetPromptReplaceEnabled toggles system-prompt replacement at runtime.
func (d *Driver) SetPromptReplaceEnabled(enabled bool) { d.promptReplace.Store(enabled) }

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelWorkBuddy }

// ParseCreds decodes a credential blob.
func ParseCreds(raw []byte) (*Creds, error) {
	var c Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("workbuddy: parse creds: %w", err)
	}
	return &c, nil
}

// LegacyAuth is the workbuddy2api-panel auths/workbuddy-*.json shape; import
// accepts it for zero-friction migration of existing accounts.
type LegacyAuth struct {
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Realm        string `json:"realm"`
		DeviceToken  string `json:"device_token"`
	} `json:"auth"`
}

// ParseLegacyAuth converts the panel's credential file into the canonical
// Creds shape.
func ParseLegacyAuth(raw []byte) (*Creds, error) {
	var la LegacyAuth
	if err := json.Unmarshal(raw, &la); err != nil {
		return nil, fmt.Errorf("workbuddy: parse legacy auth: %w", err)
	}
	if la.Auth.AccessToken == "" {
		return nil, fmt.Errorf("workbuddy: legacy auth missing accessToken")
	}
	return &Creds{
		AccessToken:  la.Auth.AccessToken,
		RefreshToken: la.Auth.RefreshToken,
		ExpiresAt:    la.Auth.ExpiresAt,
		UID:          la.Account.UID,
		Nickname:     la.Account.Nickname,
		EnterpriseID: la.Account.EnterpriseID,
		Realm:        la.Auth.Realm,
		DeviceToken:  la.Auth.DeviceToken,
	}, nil
}

func (d *Driver) chatBase(c *Creds) string {
	if c.RealmOrDefault() == "global" {
		return strings.TrimRight(d.cfg.ChatBaseGL, "/")
	}
	return strings.TrimRight(d.cfg.ChatBaseCN, "/")
}

// Stream implements driver.Driver. Upstream is forced to stream:true; the
// gateway aggregates for non-streaming clients.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	creds, err := ParseCreds(call.Account.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	if creds.AccessToken == "" {
		return nil, uperr.New(uperr.ClassAuth, 401, "workbuddy: missing access token")
	}

	body, err := d.buildBody(call, creds, false)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "workbuddy: build body: %v", err)
	}
	resp, err := d.postChat(ctx, creds, call, body)
	if err != nil {
		// 11128 "illegal API invocation from an unapproved channel" is the
		// channel check refusing the caller's *system prompt* — not the
		// account, and not something another account would accept. The edge
		// only inspects system and assistant messages, so the same text
		// passes once it arrives as the user's first turn (BP-93, following
		// magpie-community/plugins workbuddy 0.1.29's field findings on both
		// official builds).
		if ue, ok := err.(*uperr.Error); ok && ue.Class == uperr.ClassBadRequest &&
			strings.Contains(ue.Message, "11128") {
			moved, berr := d.buildBody(call, creds, true)
			if berr == nil {
				log.Printf("workbuddy: 11128 channel refusal; resending with the system prompt as the user's instructions")
				resp, err = d.postChat(ctx, creds, call, moved)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return d.parseStream(ctx, streamBody, call, emit)
}

// postChat sends one chat request and returns the open response body on 200.
// Any non-200 is classified into *uperr.Error.
func (d *Driver) postChat(ctx context.Context, creds *Creds, call driver.ChatCall, body []byte) (*http.Response, error) {
	url := d.chatBase(creds) + "/v2/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.DeviceToken != "" {
		req.Header.Set("X-Device-Token", creds.DeviceToken)
	}
	d.applyClientFingerprint(req, creds, call)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "workbuddy: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw, call.UpstreamModel)
	}
	return resp, nil
}

// buildBody renders the upstream request. systemAsUser moves the caller's
// system prompt into the first user turn inside <instructions> tags and puts
// the upstream's own default system line first — the shape the channel check
// accepts when it has refused the original (BP-93).
func (d *Driver) buildBody(call driver.ChatCall, creds *Creds, systemAsUser bool) ([]byte, error) {
	req := call.Request
	// Empty-name tool calls poison the session upstream-side (business error
	// 11133): a stream that ends before the name fragment arrives yields
	// name:"" entries, clients persist that history, and every resend is
	// rejected. Drop such calls AND their paired tool results so the
	// conversation self-heals (upstream fix 535e19e, symmetric pruning).
	badIDs := map[string]bool{}
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && strings.TrimSpace(tc.Name) == "" {
				badIDs[tc.ID] = true
			}
		}
	}
	// Rebuild the message array with prompt replacement applied.
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == ir.RoleTool && badIDs[m.ToolCallID] {
			continue
		}
		wire := map[string]any{"role": string(m.Role)}
		if m.Role == ir.RoleTool {
			// OpenAI tool messages carry a plain string; image parts are not
			// representable here and must not leak into the request (BP-70).
			wire["content"] = ir.TextOf(m.Content)
		} else if len(m.Content) == 1 && m.Content[0].Kind == "text" {
			wire["content"] = m.Content[0].Text
		} else {
			var parts []map[string]any
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
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				if badIDs[tc.ID] || strings.TrimSpace(tc.Name) == "" {
					continue
				}
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
				})
			}
			if len(tcs) > 0 {
				wire["tool_calls"] = tcs
			}
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		if m.ToolName != "" {
			wire["name"] = m.ToolName
		}
		// An assistant message that carried only pruned tool calls and no
		// content would serialize as {"role":"assistant"} — strictly-validated
		// upstreams reject that. Drop the empty shell entirely.
		if m.Role == ir.RoleAssistant && m.ToolCallID == "" &&
			len(m.ToolCalls) > 0 && wire["tool_calls"] == nil && emptyAssistantContent(wire["content"]) {
			continue
		}
		messages = append(messages, wire)
	}

	mode := strings.ToLower(d.cfg.PromptMode)
	if mode == "" {
		mode = "custom"
	}
	if systemAsUser {
		// The retry must carry the caller's OWN system text, moved — not the
		// replaced or stripped version — or the agent's instructions are lost
		// and the model behaves like a different agent (BP-93).
		messages = moveSystemToUser(messages)
	} else if mode == "custom" && d.promptReplace.Load() {
		messages = prompt.ReplaceSystem(messages, prompt.DefaultSystemPrompt)
	} else {
		messages = prompt.StripSystem(messages)
	}

	body := map[string]any{
		"model":    call.UpstreamModel,
		"messages": messages,
		"stream":   true, // forced: upstream rejects non-stream
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				// Type every enum before sending: this upstream answers 400
				// code 11133 for an enum with no type or with several types,
				// which MCP servers emit routinely (2026-10-21).
				params := t.Parameters
				if prompt.FixEnumTypes(params) {
					log.Printf("workbuddy: typed enum(s) in tool %q schema", t.Name)
				}
				fn["parameters"] = params
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
		if tc, ok := req.ToolChoice.(string); ok {
			body["tool_choice"] = tc // string form only; upstream rejects objects
		}
	}
	if req.RequestedEffort != "" {
		// upstream effort gears: minimal|low|medium|high (invalid values are
		// downgraded upstream-side to the model default). The international
		// (realm=global) deployment only accepts "high": sending low/max there
		// is rejected upstream, so it is pinned (misakano upstream parity,
		// 2026-10-21).
		effort := req.RequestedEffort
		if creds.RealmOrDefault() == "global" {
			effort = "high"
		}
		body["reasoning_effort"] = effort
	}
	// prompt_cache_key injection (per-account stable key, panel parity).
	prompt.InjectPromptCacheKey(body, creds.UID, sessionFromReq(req))

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return prompt.SanitizeBody(raw), nil
}

// moveSystemToUser rewrites the conversation so the caller's system prompt is
// read to the model as the first thing the user says, inside <instructions>
// tags, with the upstream's own default system line first.
//
// Why: WorkBuddy's channel check reads system and assistant messages only and
// refuses the whole request (400 code 11128 "Illegal API invocation from an
// unapproved channel") when it recognizes an agent identity line there. The
// identical text in a user message, a user message's parts, or a tool result
// passes. Both official builds also insist the first message IS a system
// prompt, hence the neutral lead (BP-93, field findings recorded by
// magpie-community/plugins workbuddy 0.1.29).
//
// A system message further down the conversation becomes a user message where
// it stands, so nothing is silently dropped.
func moveSystemToUser(messages []map[string]any) []map[string]any {
	if len(messages) == 0 {
		return messages
	}
	var lead []string
	i := 0
	for ; i < len(messages); i++ {
		if role, _ := messages[i]["role"].(string); role != "system" {
			break
		}
		lead = append(lead, messageText(messages[i]["content"]))
	}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, map[string]any{"role": "system", "content": prompt.DegradedSystemPrompt})
	for _, m := range messages[i:] {
		if role, _ := m["role"].(string); role == "system" {
			m = map[string]any{"role": "user", "content": messageText(m["content"])}
		}
		out = append(out, m)
	}
	said := strings.TrimSpace(strings.Join(lead, "\n\n"))
	if said != "" {
		block := "<instructions>\n" + said + "\n</instructions>"
		at := -1
		for j, m := range out {
			if role, _ := m["role"].(string); role == "user" {
				at = j
				break
			}
		}
		if at < 0 {
			out = append(out, map[string]any{"role": "user", "content": block})
		} else {
			switch c := out[at]["content"].(type) {
			case string:
				out[at] = map[string]any{"role": "user", "content": block + "\n\n" + c}
			case []map[string]any:
				out[at] = map[string]any{"role": "user",
					"content": append([]map[string]any{{"type": "text", "text": block}}, c...)}
			default:
				out[at] = map[string]any{"role": "user", "content": block}
			}
		}
	}
	return out
}

// messageText flattens a wire message's content to text.
func messageText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []map[string]any:
		var parts []string
		for _, p := range c {
			if t, ok := p["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// emptyAssistantContent reports whether the serialized content carries no
// text at all: nil, an empty part array, or whitespace-only text parts.
func emptyAssistantContent(v any) bool {
	switch c := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(c) == ""
	case []map[string]any:
		if len(c) == 0 {
			return true
		}
		for _, p := range c {
			if t, ok := p["text"].(string); ok && strings.TrimSpace(t) != "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func sessionFromReq(req *ir.ChatRequest) string {
	if req.SessionKey != "" {
		return req.SessionKey
	}
	// Deliberately no surrogate. The old fallback returned "t<len(text)>",
	// i.e. the message LENGTH — unrelated conversations with equal-length
	// prompts shared one upstream cache slot (2026-10-20 cross-conversation
	// contamination). No stable identity means no cache key at all.
	return ""
}

// parseStream consumes the upstream SSE stream.
func (d *Driver) parseStream(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)

	out := &ir.ChatResponse{
		Model:     call.Request.Model,
		CreatedAt: time.Now().Unix(),
		Message:   ir.Message{Role: ir.RoleAssistant},
	}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		toolAcc    = map[int]*ir.ToolCall{}
		toolOrder  []int
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Non-SSE error frame (upstream sometimes inlines JSON errors).
			if strings.HasPrefix(line, "{") {
				if code, msg, ok := detectBusinessError([]byte(line)); ok {
					return nil, classifyBusiness(code, msg, call.UpstreamModel)
				}
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminated = true
			break
		}
		// Business-error frames arrive as HTTP 200 with an error body.
		if code, msg, ok := detectBusinessError([]byte(payload)); ok {
			return nil, classifyBusiness(code, msg, call.UpstreamModel)
		}
		var chunk struct {
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
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int      `json:"prompt_tokens"`
				CompletionTokens int      `json:"completion_tokens"`
				Credit           *float64 `json:"credit"`
				// Prefix-cache accounting (DeepSeek/GLM convention): the hit
				// size is what the panel's cache-rate column divides by the
				// prompt size — observed live on this upstream (BP-53).
				PromptCacheHitTokens  *int `json:"prompt_cache_hit_tokens"`
				PromptCacheMissTokens *int `json:"prompt_cache_miss_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				thinkBuf.WriteString(ch.Delta.ReasoningContent)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ch.Delta.ReasoningContent}); err != nil {
					return nil, err
				}
			}
			if ch.Delta.Content != "" {
				textBuf.WriteString(ch.Delta.Content)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ch.Delta.Content}); err != nil {
					return nil, err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := toolAcc[tc.Index]
				if !ok {
					acc = &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					toolAcc[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
					if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: tc.Index, ToolCall: &ir.ToolCall{ID: acc.ID, Name: acc.Name}}); err != nil {
						return nil, err
					}
				}
				if tc.Function.Arguments != "" {
					acc.Arguments += tc.Function.Arguments
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: tc.Index, Delta: tc.Function.Arguments}); err != nil {
						return nil, err
					}
				}
			}
			if ch.FinishReason != "" {
				terminated = true
				stop = mapFinish(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			usage = ir.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
			if chunk.Usage.Credit != nil {
				usage.Credit = *chunk.Usage.Credit
				usage.CreditKnown = true
			}
			if chunk.Usage.PromptCacheHitTokens != nil {
				usage.CachedInputTokens = *chunk.Usage.PromptCacheHitTokens
				usage.CacheKnown = true
			} else if chunk.Usage.PromptCacheMissTokens != nil && chunk.Usage.PromptTokens > 0 {
				// Miss-only frame: the hit size is the remainder of the prompt.
				usage.CacheKnown = true
				if hit := chunk.Usage.PromptTokens - *chunk.Usage.PromptCacheMissTokens; hit > 0 {
					usage.CachedInputTokens = hit
				}
			}
			gotUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"workbuddy: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy: stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy: stream ended without terminal signal")
	}

	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *toolAcc[idx])
	}
	if len(out.Message.ToolCalls) > 0 && stop == ir.StopEnd {
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.StopReason = stop
	out.Usage = usage
	if gotUsage {
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &usage}); err != nil {
			return nil, err
		}
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: stop}); err != nil {
		return nil, err
	}
	return out, nil
}

func mapFinish(reason string) ir.StopReason {
	switch strings.ToLower(reason) {
	case "stop":
		return ir.StopEnd
	case "length":
		return ir.StopLength
	case "tool_calls":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default:
		return ir.StopEnd
	}
}

// detectBusinessError finds a business code + message in a JSON body.
//
// Message fields: upstream billing envelopes carry the text in `msg`
// (agentpoints apiEnvelope parity); SSE/OpenAI-style bodies use `message`
// or nested error.message. All three are read — dropping `msg` used to lose
// every billing error's text (ticket 30: the checkin duplicate rejection
// arrived as code=10001 with the reason only in `msg`).
func detectBusinessError(raw []byte) (int, string, bool) {
	var doc struct {
		Code    any    `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Error   struct {
			Code    any    `json:"code"`
			Msg     string `json:"msg"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, "", false
	}
	pick := func(msg, message string) string {
		if msg != "" {
			return msg
		}
		return message
	}
	code := toInt(doc.Code)
	msg := pick(doc.Msg, doc.Message)
	if code == 0 {
		code = toInt(doc.Error.Code)
		msg = pick(doc.Error.Msg, doc.Error.Message)
	}
	if code == 0 {
		return 0, "", false
	}
	return code, msg, true
}

func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n := 0
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	default:
		return 0
	}
}

// classifyBusiness maps a business code to the error taxonomy.
func classifyBusiness(code int, msg, upstreamModel string) *uperr.Error {
	switch code {
	case codeModelRate:
		e := uperr.New(uperr.ClassModelRate, 200, "workbuddy: %s", msg)
		e.Model = upstreamModel
		e.Code = fmt.Sprint(code)
		e.ResetAt = parseResetWallClock(msg)
		return e
	case codeQueueFull:
		// Upstream capacity, not the account: no rotation, no account
		// penalty; the pool marks the channel+model busy so follow-up
		// requests fail fast until the queue drains.
		e := uperr.New(uperr.ClassUpstreamBusy, 429, "workbuddy: upstream queue full: %s", msg)
		e.Model = upstreamModel
		return e
	case codeBadParams:
		return uperr.New(uperr.ClassBadRequest, 400, "workbuddy: %s", msg)
	case codeChannelRefused:
		// The channel check refused the caller's system prompt. Request-level:
		// no account penalty, and the driver retries once with the prompt moved
		// into the user turn (BP-93). The code stays in the message so the
		// retry can recognize it.
		return uperr.New(uperr.ClassBadRequest, 400, "workbuddy: 11128 channel refused: %s", msg)
	case codeAccountRestricted:
		// Same code carries two meanings; the wording decides (see the
		// constant's comment). Rate-limit wording keeps the account usable.
		lowerMsg := strings.ToLower(msg)
		if strings.Contains(lowerMsg, "rate") || strings.Contains(lowerMsg, "limit") {
			e := uperr.New(uperr.ClassSoftRate, 429, "workbuddy: %s", msg)
			e.ResetAt = parseResetWallClock(msg)
			return e
		}
		// "request illegal" / "account restricted": the account is banned
		// upstream. Rotate away and stop selecting it.
		return uperr.New(uperr.ClassAccountRestricted, 403, "workbuddy: account restricted by upstream: %s", msg)
	case codeSessionDead:
		return uperr.New(uperr.ClassAuth, 401, "workbuddy: %s", msg)
	default:
		// Business-code branch runs before the status fallbacks, so a
		// request-level rejection must be recognized HERE too — otherwise
		// code 11115 ("prompt is too long") lands in the generic internal
		// bucket and the status branch never sees it.
		lowerMsg := strings.ToLower(msg)
		if code == codePromptTooLong || isPromptTooLong(lowerMsg) {
			return uperr.New(uperr.ClassBadRequest, 400, "workbuddy: prompt too long: %s", msg)
		}
		if strings.Contains(msg, "积分") || strings.Contains(lowerMsg, "credit") {
			return uperr.New(uperr.ClassHardCredit, 402, "workbuddy: %s", msg)
		}
		if strings.Contains(msg, "拦截") || strings.Contains(lowerMsg, "blocked") || strings.Contains(msg, "审核") {
			return uperr.New(uperr.ClassContentBlocked, 400, "workbuddy: %s", msg)
		}
		return uperr.New(uperr.ClassInternal, 200, "workbuddy: code=%d %s", code, msg)
	}
}

// parseResetWallClock extracts "将在 2026-10-14 15:30:00 重置" style reset
// moments from a 6004 message (Asia/Shanghai wall clock).
func parseResetWallClock(msg string) time.Time {
	layouts := []string{"2006-01-02 15:04:05", "2006/01/02 15:04:05"}
	for i := 0; i+19 <= len(msg); i++ {
		seg := msg[i : i+19]
		for _, layout := range layouts {
			if t, err := time.ParseInLocation(layout, seg, time.FixedZone("CST", 8*3600)); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// isPromptTooLong reports the upstream's over-context marker: business code
// 11115 or the literal wording. The caller passes a lowercased body. Both the
// JSON-envelope shape and the plain-text shape are covered because the edge
// sometimes answers with a bare message instead of an envelope.
func isPromptTooLong(lowerBody string) bool {
	if strings.Contains(lowerBody, "11115") {
		return true
	}
	return strings.Contains(lowerBody, "prompt is too long") ||
		strings.Contains(lowerBody, "context length exceeded") ||
		strings.Contains(lowerBody, "context_too_long")
}

// applyClientFingerprint sends the official WorkBuddy desktop client's header
// family on chat requests.
//
// Why: the chat edge classifies traffic. Requests carrying only the four
// transport headers (content-type/auth/Accept/device) look like a script, and
// upstream later marks the ACCOUNT as "unusual activity" (403 code=11140
// "request illegal", displayMsg 账号存在异常行为，已被限制使用 — field
// diagnosis 2026-10-20). The reference projects send the same family on every
// call for that reason.
//
// Values are stable derivations from the account uid/session key, never random
// per request: the official client reuses one conversation id per
// conversation, so rotating them would look *more* suspicious. Their *shapes*
// matter too — see identity.go (BP-90).
//
// Set channels.workbuddy.disable_client_fingerprint to turn this off.
func (d *Driver) applyClientFingerprint(req *http.Request, creds *Creds, call driver.ChatCall) {
	if d.cfg.DisableClientFingerprint {
		return
	}
	global := creds.RealmOrDefault() == "global"
	origin, lang := "https://www.codebuddy.cn", "zh-CN"
	if global {
		origin, lang = "https://www.workbuddy.ai", "en-US"
	}
	req.Header.Set("User-Agent", workBuddyUA(global))
	req.Header.Set("X-CodeBuddy-Request", "1")
	// X-Product is the SKU segment: the real client always sends "SaaS" and
	// carries the product name in X-IDE-Name/Type instead. Sending "WorkBuddy"
	// here is a same-header-different-value tell — easier to match on than an
	// extra header would be (BP-90, official captures of both packages).
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", workBuddyClientVersion(global))
	req.Header.Set("X-Domain", strings.TrimPrefix(origin, "https://"))
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	// Browser/CLI-shaped headers the desktop client never sends on chat:
	// Origin, Referer, Accept-Language, X-Machine-ID, X-Session-ID,
	// X-Requested-With and the X-No-* sentinels all came from the CodeBuddy
	// CLI's conventions. Both official WorkBuddy packages omit every one of
	// them here (BP-90) — an extra header is a fingerprint in its own right.
	//
	// channels.workbuddy.send_browser_headers restores them for A/B work.
	if d.cfg.SendBrowserHeaders {
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("Accept-Language", lang)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("X-No-Enterprise-Id", "1")
		machine := stableHex(uidSeed(creds, call) + "|machine")
		req.Header.Set("X-Machine-ID", machine)
		req.Header.Set("X-Session-ID", machine)
	}
	// The openai-node SDK / agent segment the client also sends.
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Package-Version", stainlessSDKVersion)
	req.Header.Set("X-Stainless-OS", "Windows")
	req.Header.Set("X-Stainless-Arch", "x64")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", stainlessNodeVersion)
	req.Header.Set("X-Stainless-Retry-Count", "0")

	// Conversation header family. Every id here has the shape the real client
	// sends: a dashed UUID v4 for the conversation, millisecond-prefixed 32-hex
	// for per-message ids, and unrelated random values for trace/span.
	seed := uidSeed(creds, call)
	sk := call.Request.SessionKey
	var conv, convReq, acp string
	if sk != "" {
		conv = stableUUIDV4(seed, sk, "")
		// Turn-level aggregation key: stable per conversation, and a different
		// value from the conversation id (the client keeps them apart).
		convReq = stableUUIDV4(seed, sk, "turn")
		acp = stableUUIDV4(seed, sk, "acp")
	} else {
		// No stable conversation identity: mint fresh ids. A constant fallback
		// told upstream that every unrelated conversation was the same one —
		// the 2026-10-20 field bug where two ZCode sessions shared a
		// conversation id and the model's answers/images crossed over.
		conv, convReq, acp = randomUUIDV4(), randomUUIDV4(), randomUUIDV4()
	}
	msg := newMessageID()
	trace := newTraceID()
	span := newSpanID()
	parent := newSpanID()
	req.Header.Set("X-Conversation-ID", conv)
	req.Header.Set("X-Conversation-Request-ID", convReq)
	req.Header.Set("X-Conversation-Message-ID", msg)
	req.Header.Set("X-Request-ID", msg)
	req.Header.Set("X-Root-Request-ID", convReq)
	// Trace and request ids never correlate on the real client: X-Trace-ID is
	// its own random value and B3 follows the trace, not the request.
	req.Header.Set("X-Trace-ID", trace)
	req.Header.Set("X-B3-TraceId", trace)
	req.Header.Set("X-B3-SpanId", span)
	req.Header.Set("X-B3-ParentSpanId", parent)
	req.Header.Set("X-B3-Sampled", "1")
	// The client also sends the single-header b3 and W3C traceparent forms.
	// Sending only the X-B3-* trio is a shape it never produces.
	req.Header.Set("b3", trace+"-"+span+"-1-"+parent)
	req.Header.Set("traceparent", "00-"+trace+"-"+span+"-01")
	req.Header.Set("acp-connection-id", acp)
}

// uidSeed is the per-account isolation seed for derived ids.
func uidSeed(creds *Creds, call driver.ChatCall) string {
	if creds.UID != "" {
		return creds.UID
	}
	if call.Account != nil {
		return call.Account.ID
	}
	return ""
}

// stableHex derives a 32-hex id from a seed (deterministic per account/session,
// never per request).
func stableHex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// randomHex returns a fresh 32-hex id for requests that carry no stable
// conversation identity.
func randomHex() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:16])
	}
	return hex.EncodeToString(b[:])
}

// classifyHTTP maps HTTP status + body to the taxonomy.
func classifyHTTP(status int, body []byte, upstreamModel string) *uperr.Error {
	if code, msg, ok := detectBusinessError(body); ok {
		return classifyBusiness(code, msg, upstreamModel)
	}
	text := string(body)
	lower := strings.ToLower(text)
	// Account restriction without a JSON envelope: the edge sometimes answers
	// bare text. This must be judged before the generic 403 → WAF branch,
	// because a restricted account has to leave routing (2026-10-20).
	if status == 403 && (strings.Contains(lower, "request illegal") ||
		strings.Contains(lower, "11140") ||
		strings.Contains(lower, "account restricted") ||
		strings.Contains(text, "异常行为")) {
		return uperr.New(uperr.ClassAccountRestricted, status, "workbuddy: account restricted by upstream: %s", truncate(body, 300))
	}
	// Prompt-too-long (11115) is request-level and is judged before the
	// status fallbacks: upstream may express it with 400, 404 or 413, and on
	// 404 the generic branch would otherwise file it as a dead route.
	// Rotating accounts cannot shorten a prompt.
	if status == 400 || status == 404 || status == 413 {
		if isPromptTooLong(lower) {
			return uperr.New(uperr.ClassBadRequest, status, "workbuddy: prompt too long: %s", truncate(body, 300))
		}
	}
	switch {
	case status == 401:
		return uperr.New(uperr.ClassAuth, status, "workbuddy: %s", truncate(body, 300))
	case status == 402:
		return uperr.New(uperr.ClassHardCredit, status, "workbuddy: %s", truncate(body, 300))
	case status == 404:
		// The path/model does not exist upstream: a property of the binding,
		// identical for every account of this channel. Not a rate limit, so
		// the pool applies no account penalty (2026-10-19).
		return uperr.New(uperr.ClassNotFound, status, "workbuddy: %s", truncate(body, 300))
	case status == 413:
		// Payload too large is a request problem no account can fix.
		return uperr.New(uperr.ClassBadRequest, status, "workbuddy: %s", truncate(body, 300))
	case status == 429 || strings.Contains(lower, "rate") || strings.Contains(lower, "limit"):
		e := uperr.New(uperr.ClassSoftRate, status, "workbuddy: %s", truncate(body, 300))
		e.ResetAt = parseResetWallClock(text)
		return e
	case status == 403:
		if uperr.IsEdgeBlock(body) {
			return uperr.EdgeBlocked(status, "workbuddy: edge blocked: %s", truncate(body, 300))
		}
		return uperr.New(uperr.ClassWAF, status, "workbuddy: %s", truncate(body, 300))
	case status == 400:
		return uperr.New(uperr.ClassBadRequest, status, "workbuddy: %s", truncate(body, 300))
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "workbuddy: %s", truncate(body, 300))
	default:
		return uperr.New(uperr.ClassInternal, status, "workbuddy: %s", truncate(body, 300))
	}
}

// --- credits / probe / checkin ---

// BillingResource mirrors get-user-resource's per-package rows.
type billingPackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleEndTime        string `json:"CycleEndTime"`
	ExpiredTime         string `json:"ExpiredTime"`
	// DeductionEndTime (epoch ms): the moment the package can no longer be
	// deducted — the REAL "use it or lose it" expiry. The upstream deducts
	// packages FEFO by this moment (agentpoints acb3830; CycleEndTime is the
	// period boundary and only a fallback).
	DeductionEndTime int64  `json:"DeductionEndTime"`
	PackageEndTime   string `json:"PackageEndTime"`
}

// parseCredits aggregates balance + expiring buckets from a
// get-user-resource response body. Accepts both the enveloped form
// {"code":0,"data":{"Response":...}} and the bare {"Response":...} form.
func parseCredits(raw []byte, soon time.Duration) (*model.Credits, error) {
	type accountsBody struct {
		Response struct {
			Data struct {
				Accounts []billingPackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	var resp accountsBody
	parsed := false
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 && env.Data[0] == '{' {
		if err := json.Unmarshal(env.Data, &resp); err == nil {
			parsed = true
		}
	}
	if !parsed {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	out := &model.Credits{UpdatedAt: now}
	for _, p := range resp.Response.Data.Accounts {
		remain, size := packageRemainSize(p)
		if remain < 0 {
			remain = 0
		}
		out.Remain += remain
		out.Total += size
		if remain <= 0 {
			continue
		}
		end := parsePackageEnd(p)
		if end.IsZero() || !end.After(now) {
			continue
		}
		if out.EarliestExpiresAt.IsZero() || end.Before(out.EarliestExpiresAt) {
			out.EarliestExpiresAt = end
			out.EarliestRemaining = remain
		} else if end.Equal(out.EarliestExpiresAt) {
			out.EarliestRemaining += remain
		}
		if soon > 0 && !end.After(now.Add(soon)) {
			out.Expiring += remain
		}
	}
	return out, nil
}

func packageRemainSize(p billingPackage) (int64, int64) {
	if p.CycleCapacitySize > 0 {
		remain := p.CycleCapacityRemain
		if remain < 0 {
			remain = 0
		}
		return remain, p.CycleCapacitySize
	}
	remain := p.CapacityRemain
	if remain < 0 {
		remain = 0
	}
	size := p.CapacitySize
	if size < remain {
		size = remain
	}
	return remain, size
}

// parsePackageEnd resolves a package's real expiry. Priority: DeductionEndTime
// (epoch ms — the deductible-window end, what the upstream actually deducts
// FEFO by), then the wall-clock string fields ExpiredTime → PackageEndTime →
// CycleEndTime (period boundary, last resort). Ticket 34, agentpoints
// acb3830: judging "use it or lose it" by CycleEndTime misdates the expiry.
func parsePackageEnd(p billingPackage) time.Time {
	if p.DeductionEndTime > 0 {
		return time.UnixMilli(p.DeductionEndTime)
	}
	layouts := []string{"2006-01-02 15:04:05", time.RFC3339}
	for _, raw := range []string{p.ExpiredTime, p.PackageEndTime, p.CycleEndTime} {
		if raw == "" {
			continue
		}
		for _, layout := range layouts {
			if t, err := time.ParseInLocation(layout, raw, time.FixedZone("CST", 8*3600)); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// Probe implements driver.Prober: fetch credit balance with expiry buckets.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	body := map[string]any{
		"PageNumber":  1,
		"PageSize":    100,
		"ProductCode": "p_tcaca",
		"Status":      []int{0, 3},
	}
	raw, err := d.billingCall(ctx, creds, "/v2/billing/meter/get-user-resource", body, 1)
	if err != nil {
		return nil, err
	}
	credits, err := parseCredits(raw, 7*24*time.Hour)
	if err != nil {
		return nil, uperr.New(uperr.ClassInternal, 0, "workbuddy: credits parse: %v", err)
	}
	return &driver.ProbeResult{Credits: credits, Note: "credit balance refreshed"}, nil
}

// CreditPackages lists the account's credit packs with per-pack expiry
// (panel 国模积分情况 page; same billing endpoint as Probe).
func (d *Driver) CreditPackages(ctx context.Context, acc *model.Account) ([]driver.Package, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	body := map[string]any{
		"PageNumber":  1,
		"PageSize":    100,
		"ProductCode": "p_tcaca",
		"Status":      []int{0, 3},
	}
	raw, err := d.billingCall(ctx, creds, "/v2/billing/meter/get-user-resource", body, 1)
	if err != nil {
		return nil, err
	}
	packs, err := parsePackages(raw)
	if err != nil {
		return nil, uperr.New(uperr.ClassInternal, 0, "workbuddy: packages parse: %v", err)
	}
	return packs, nil
}

// parsePackages extracts the raw billing rows with per-pack expiry.
func parsePackages(raw []byte) ([]driver.Package, error) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []billingPackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	parsed := false
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 && env.Data[0] == '{' {
		if err := json.Unmarshal(env.Data, &resp); err == nil {
			parsed = true
		}
	}
	if !parsed {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
	}
	out := make([]driver.Package, 0, len(resp.Response.Data.Accounts))
	for _, p := range resp.Response.Data.Accounts {
		remain, size := packageRemainSize(p)
		out = append(out, driver.Package{
			Name:     p.PackageName,
			Remain:   remain,
			Size:     size,
			ExpireAt: parsePackageEnd(p),
		})
	}
	return out, nil
}

// IsAlreadyCheckin reports whether err is the upstream's idempotent
// "already checked in today" rejection: a duplicate daily checkin gets
// code=10001/14001 with the reason in the message (已签到 / "already").
// Callers treat it as success — the task ledger used to record a FAILED row
// for every repeat slot of the day (ticket 30).
func IsAlreadyCheckin(err error) bool {
	if err == nil {
		return false
	}
	ue, ok := uperr.As(err)
	if !ok {
		return false
	}
	if strings.Contains(ue.Message, "已签到") {
		return true
	}
	return strings.Contains(strings.ToLower(ue.Message), "already")
}

// Checkin performs the daily checkin (idempotent upstream-side).
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	_, err = d.billingCall(ctx, creds, "/v2/billing/meter/daily-checkin", map[string]any{}, 0)
	return err
}

// ReportEvent posts a telemetry event to the upstream report endpoint.
func (d *Driver) ReportEvent(ctx context.Context, acc *model.Account, event map[string]any) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	_, err = d.billingCall(ctx, creds, "/v2/report", event, 0)
	return err
}

// billingCall posts a JSON body to a billing/report endpoint with one
// transient retry (upstream 5xx right after checkin is a known flake).
func (d *Driver) billingCall(ctx context.Context, creds *Creds, path string, body any, retries int) ([]byte, error) {
	raw, _ := json.Marshal(body)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.chatBase(creds)+path, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		if creds.DeviceToken != "" {
			req.Header.Set("X-Device-Token", creds.DeviceToken)
		}
		resp, err := d.rpc.Do(req)
		if err != nil {
			lastErr = uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "workbuddy: %v", err)
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		// Balance/checkin data feeds pool scoring; a truncated read must not
		// pass as a valid body (ticket 11).
		if rerr != nil {
			log.Printf("workbuddy: billing read failed (uid=%s, endpoint=%s): %v", creds.UID, path, rerr)
			lastErr = uperr.New(uperr.ClassTransport, 0, "workbuddy: read %s body: %v", path, rerr)
			if attempt < retries {
				time.Sleep(300 * time.Millisecond)
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return data, nil
		}
		if resp.StatusCode >= 500 && attempt < retries {
			lastErr = uperr.New(uperr.ClassTransport, resp.StatusCode, "workbuddy: %s", truncate(data, 200))
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return nil, classifyHTTP(resp.StatusCode, data, "")
	}
	return nil, lastErr
}

// refreshPath is the real token-refresh route. The previously hardcoded
// "/v2/plugin/auth/refresh" does not exist upstream (verified 2026-10-18:
// 404 Route Not Found), so a dead token could never be recovered in-place and
// the account stayed broken until a manual re-login.
const refreshPath = "/v2/plugin/auth/token/refresh"

// Refresh refreshes the access token when the refresh token flow is
// available. WorkBuddy issues long-lived tokens (60d) via the device flow;
// this endpoint refreshes them ahead of expiry.
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	return d.refresh(ctx, acc, false)
}

// RefreshNow forces a token refresh regardless of the local expiry clock.
// Needed when upstream rejects a token that has not expired yet (revoked
// elsewhere, or a session killed server-side): gating on ExpiresAt would skip
// exactly those accounts.
func (d *Driver) RefreshNow(ctx context.Context, acc *model.Account) error {
	return d.refresh(ctx, acc, true)
}

func (d *Driver) refreshOnce(ctx context.Context, creds *Creds) ([]byte, error) {
	// The route takes the refresh token as a header and no body (verified
	// 2026-10-18: empty body → 400 "refreshToken is empty" when the header is
	// missing; a bad header value → 401 code 12153).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.chatBase(creds)+refreshPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("X-Refresh-Token", creds.RefreshToken)
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", d.chatBase(creds))
	req.Header.Set("X-Product", "SaaS")
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "workbuddy refresh: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// code 12153 = refresh token dead: only a re-login helps.
		return nil, uperr.New(uperr.ClassAuth, resp.StatusCode, "workbuddy refresh: %s", truncate(data, 200))
	}
	var env struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "workbuddy refresh: bad response")
	}
	tok := env.Data
	if tok.AccessToken == "" {
		tok = struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		}{env.AccessToken, env.RefreshToken, env.ExpiresIn}
	}
	if tok.AccessToken == "" {
		return nil, uperr.New(uperr.ClassAuth, 0, "workbuddy refresh: bad response")
	}
	creds.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		creds.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 && tok.ExpiresIn < 10*365*24*3600 {
		creds.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	updated, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Models returns the curated model list (dynamic discovery happens via
// /v3/config during probes; the static list covers the core set).
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return []string{
		"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo",
		"deepseek-v4-flash", "deepseek-v4-pro", "deepseek-v4.1-flash",
		"kimi-k2.6", "kimi-k2.7", "kimi-k3", "minimax-m3",
		"hy3", "hy3-preview", "gemini-3.5-flash",
	}, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
