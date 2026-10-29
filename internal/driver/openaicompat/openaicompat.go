// Package openaicompat implements the generic OpenAI-compatible channel: a
// plain passthrough to any standard /v1/chat/completions upstream (relay
// stations, official keys). It is the universal fallback binding.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// Driver implements driver.Driver for the openai-compat channel.
type Driver struct {
	cfg  config.OpenAICompatSettings
	http *http.Client // streaming chat calls (no total timeout + header/idle watchdogs)
}

// New builds the driver.
func New(cfg config.OpenAICompatSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	return &Driver{cfg: cfg, http: httpClient}
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelOpenAICompat }

// Stream implements driver.Driver.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	if d.cfg.BaseURL == "" {
		return nil, uperr.New(uperr.ClassInternal, 0, "openaicompat: base_url not configured")
	}
	body, err := buildBody(call)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "openaicompat: build body: %v", err)
	}
	base := strings.TrimRight(d.cfg.BaseURL, "/")
	url := base + "/chat/completions"
	if strings.HasSuffix(base, "/v1") {
		url = base + "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// User-imported accounts override the channel key; the synthetic default
	// always follows current configuration.
	apiKey := d.cfg.APIKey
	if call.Account != nil && call.Account.ID != "openaicompat-default" && len(call.Account.CredsJSON) > 0 {
		var creds struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal(call.Account.CredsJSON, &creds) == nil && creds.APIKey != "" {
			apiKey = creds.APIKey
		}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "openaicompat: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return parseSSE(ctx, streamBody, call, emit)
}

func buildBody(call driver.ChatCall) ([]byte, error) {
	req := call.Request
	var messages []map[string]any
	for _, m := range req.Messages {
		wire := map[string]any{"role": string(m.Role)}
		if m.Role == ir.RoleTool {
			wire["content"] = ir.TextOf(m.Content)
		} else if len(m.Content) == 1 && m.Content[0].Kind == "text" {
			wire["content"] = m.Content[0].Text
		} else if len(m.Content) > 0 {
			// BP-179 (magpie 4cc331b): a content list made ONLY of text parts
			// goes as the texts joined by a newline — strict OpenAI-compatible
			// backends (vLLM shapes) answer a text-part array with 422 "input
			// must be a valid string". Anything with an image stays a list.
			allText := true
			for _, p := range m.Content {
				if p.Kind != "text" {
					allText = false
					break
				}
			}
			if allText {
				texts := make([]string, 0, len(m.Content))
				for _, p := range m.Content {
					texts = append(texts, p.Text)
				}
				wire["content"] = strings.Join(texts, "\n")
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
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
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
		messages = append(messages, wire)
	}
	body := map[string]any{
		"model":    call.UpstreamModel,
		"messages": messages,
		"stream":   true,
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
	if len(req.Tools) > 0 {
		var tools []map[string]any
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
		body["tools"] = tools
	}
	if req.RequestedEffort != "" {
		body["reasoning_effort"] = req.RequestedEffort
	}
	return json.Marshal(body)
}

func parseSSE(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: time.Now().Unix(), Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		tools      = map[int]*ir.ToolCall{}
		toolOrder  []int
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "openaicompat: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		// BP-180 (new-api 55515dba): some upstreams emit the bare "[DONE]"
		// without the "data:" prefix — missing it read the whole stream as a
		// transport failure even though the turn had completed.
		if line == "[DONE]" {
			terminated = true
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminated = true
			break
		}
		// Mid-stream error frame on a 200 stream (ticket 36): count it as a
		// failure and classify the account instead of passing it as success.
		if fe := detectFrameError(payload); fe != nil {
			return nil, fe
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
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				// BP-161: cached-input counts, two shapes. OpenAI-family
				// upstreams use prompt_tokens_details.cached_tokens; DeepSeek
				// uses the flat prompt_cache_hit_tokens (its prompt_tokens
				// already sums hit+miss). Losing these zeroes the sticky
				// session's cache metrics.
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
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
				acc, ok := tools[tc.Index]
				if !ok {
					acc = &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					tools[tc.Index] = acc
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
			// BP-161: the OpenAI details shape wins, the DeepSeek flat field
			// is the fallback.
			if chunk.Usage.PromptTokensDetails != nil {
				usage.CachedInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				usage.CacheKnown = true
			} else if chunk.Usage.PromptCacheHitTokens > 0 {
				usage.CachedInputTokens = chunk.Usage.PromptCacheHitTokens
				usage.CacheKnown = true
			}
			gotUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"openaicompat: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "openaicompat: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "openaicompat: stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "openaicompat: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "openaicompat: stream ended without terminal signal")
	}

	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *tools[idx])
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

// detectFrameError surfaces an "error" object inside a 200 SSE frame
// (ticket 36): OpenAI-style upstreams can open the stream and then report a
// mid-stream error (rate limit / quota / overload). Ignoring it used to
// count a failed request as success — no penalty, sticky stayed pinned.
// Classification reuses classifyHTTP with a status derived from the frame
// (numeric code, or known type strings); unknown shapes fall back to 500
// (transport-style: rotate with a short cooldown).
func detectFrameError(payload string) *uperr.Error {
	if !strings.Contains(payload, `"error"`) {
		return nil
	}
	var doc struct {
		Error struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return nil
	}
	if doc.Error.Message == "" && doc.Error.Type == "" && doc.Error.Code == nil {
		return nil
	}
	status := 500
	if n, ok := doc.Error.Code.(float64); ok && n >= 400 && n <= 599 {
		status = int(n)
	} else {
		switch hint := strings.ToLower(doc.Error.Type + " " + toStr(doc.Error.Code)); {
		case strings.Contains(hint, "rate") || strings.Contains(hint, "limit") || strings.Contains(hint, "overload"):
			status = 429
		case minuteLimitBody(strings.ToLower(doc.Error.Message) + " " + hint):
			// A per-minute wall that avoided the words "rate"/"limit" (BP-153:
			// Gemini's "…quotaId …PerMinute" said only "quota" and used to
			// reach the 402 branch as a balance death).
			status = 429
		case strings.Contains(hint, "quota") || strings.Contains(hint, "insufficient"):
			status = 402
		case strings.Contains(hint, "invalid") || strings.Contains(hint, "param"):
			status = 400
		case strings.Contains(hint, "auth"):
			status = 401
		}
	}
	return classifyHTTP(status, []byte(payload))
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// minuteMarkers name a per-minute / per-second window: the frequency-family
// 429 (magpie d83f887a, sub2api #8286). Vendors word them as quotas —
// Gemini's "…quotaId …PerMinute", Vertex's "requests_per_minute…",
// DashScope's "Allocated quota exceeded…" — so a bare "quota" must never
// turn one into a balance death.
var urlNoise = regexp.MustCompile(`https?://\S+`)

var minuteMarkers = regexp.MustCompile(
	`(?i)\bper[ -_]?minute\b|\bper[ -_]?second\b|\bperminute\b|requests?_per_minute|tokens? per minute|` +
		`\b(tpm|rpm)\b|每分钟|每秒|过于频繁|频率`)

// longWindowWords are the plan/period resets that must NOT be read as a
// minute-level throttle even when the same body also says "quota".
var longWindowWords = regexp.MustCompile(`(?i)per[ -_]?(hour|day|week|month)|monthly|weekly|daily|每小时|每天|每月`)

// minuteLimitBody reports whether an error body describes a minute-or-less
// frequency throttle. Links are stripped first: "aka.ms/oai/quotaincrease"
// inside a help URL must not decide the class.
func minuteLimitBody(lower string) bool {
	text := urlNoise.ReplaceAllString(lower, " ")
	return minuteMarkers.MatchString(text) && !longWindowWords.MatchString(text)
}

// creditShortfallWords is the credit-death wording a 400/409/422 can carry
// (BP-193, magpie 3627742 / issue #4095: "可用积分已不足…请充值" over HTTP
// 400): treating it as a client error ends the request while accounts with
// credit remain, and never cools the empty one. Any status with this wording
// is the balance death instead.
var creditShortfallWords = regexp.MustCompile(
	`积分.{0,12}(不足|用完|耗尽)|不足.{0,8}积分|请充值|(?i)insufficient.{0,24}credit|(?i)credit.{0,24}(insufficient|exhausted|has run out|balance)|(?i)out of credits?\b`)

// modelUnserved401 / authLapsed401 separate a 401 that names the MODEL as the
// problem (BP-199, magpie 3ec2552 / plugins #468: "The model X is not
// available on your plan" — the account is fine) from one that names the
// credential. The lapsed wording wins when both appear.
var modelUnserved401 = regexp.MustCompile(
	`(?i)model[^.\n]{0,48}(not available|not supported|not entitled|no access|not enabled|unavailable)|(?i)(unsupported|unknown) model|模型(不支持|未开通|无权(访问|使用))`)
var authLapsed401 = regexp.MustCompile(
	`(?i)invalid.{0,16}(api.?key|token)|(?i)(expired|unauthorized|authentication failed|sign.?in|credential)|api.?key|令牌|鉴权`)

// contentPolicy403 is the wording third-party gateways put on a content
// rejection with HTTP 403 (BP-200, magpie 750bd1b): clients read 403 as an
// auth failure and tell the user to log in again. It is a request problem —
// ClassContentBlocked (400, no account penalty), not ClassWAF.
var contentPolicy403 = regexp.MustCompile(
	`(?i)content_policy_rejected|(?i)content[_ ]?filter|(?i)content policy|内容安全|内容审核|合规审查`)

func classifyHTTP(status int, body []byte) *uperr.Error {
	text := string(body)
	lower := strings.ToLower(text)
	switch {
	case status == 401:
		// BP-199: a 401 whose body blames the model (plan/entitlement) and
		// never the credential is a request problem on a healthy account —
		// cooling it for auth would bench it for 10 minutes over a routing
		// mistake.
		if modelUnserved401.MatchString(text) && !authLapsed401.MatchString(text) {
			return uperr.WithFields(uperr.New(uperr.ClassBadRequest, status, "openaicompat: model not served by this credential: %s", truncate(body, 300)), body)
		}
		return uperr.WithFields(uperr.New(uperr.ClassAuth, status, "openaicompat: %s", truncate(body, 300)), body)
	case status == 429:
		e := uperr.WithFields(uperr.New(uperr.ClassSoftRate, status, "openaicompat: %s", truncate(body, 300)), body)
		// A frequency-shaped 429 asks for seconds (sub2api #8286), not the
		// pool's 10-minute soft base. DashScope words its TPM wall
		// "Allocated quota exceeded / increase your quota limit" with no
		// window word at all (magpie d83f887a); that phrase is only trusted
		// under a 429 status — as a 402 it keeps meaning a balance death.
		if minuteLimitBody(lower) || strings.Contains(lower, "allocated quota exceeded") || strings.Contains(lower, "increase your quota limit") {
			e.ResetAt = time.Now().Add(5 * time.Second)
		}
		return e
	case minuteLimitBody(lower):
		// A frequency throttle wearing quota wording (BP-153): rest for a
		// few seconds, not the balance death the old quota match gave it.
		e := uperr.New(uperr.ClassSoftRate, status, "openaicompat: %s", truncate(body, 300))
		e.ResetAt = time.Now().Add(5 * time.Second)
		return uperr.WithFields(e, body)
	case status == 402 || creditShortfallWords.MatchString(text) || strings.Contains(lower, "insufficient") || strings.Contains(text, "余额"):
		// BP-193: the shortfall wording decides over the status — a 400
		// carrying it is the same balance death and must rotate to accounts
		// that still have credit.
		return uperr.WithFields(uperr.New(uperr.ClassHardCredit, status, "openaicompat: %s", truncate(body, 300)), body)
	case status == 403:
		// A CDN/WAF edge block is not this account's fault: every account
		// behind the same egress hits it, so charging them one by one turns
		// one edge rule into a dead pool (BP-94).
		if uperr.IsEdgeBlock(body) {
			return uperr.WithFields(uperr.EdgeBlocked(status, "openaicompat: edge blocked: %s", truncate(body, 300)), body)
		}
		// BP-200: a content-policy rejection wears 403 but is a request
		// problem; as ClassWAF it would cool a healthy account for wording
		// the account has no influence over.
		if contentPolicy403.MatchString(text) {
			return uperr.WithFields(uperr.New(uperr.ClassContentBlocked, status, "openaicompat: content policy: %s", truncate(body, 300)), body)
		}
		return uperr.WithFields(uperr.New(uperr.ClassWAF, status, "openaicompat: %s", truncate(body, 300)), body)
	case status == 400 || status == 404:
		return uperr.WithFields(uperr.New(uperr.ClassBadRequest, status, "openaicompat: %s", truncate(body, 300)), body)
	case status >= 500:
		return uperr.WithFields(uperr.New(uperr.ClassTransport, status, "openaicompat: %s", truncate(body, 300)), body)
	default:
		return uperr.WithFields(uperr.New(uperr.ClassInternal, status, "openaicompat: %s", truncate(body, 300)), body)
	}
}

// Models returns the configured model list.
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	if len(d.cfg.Models) > 0 {
		return d.cfg.Models, nil
	}
	return nil, fmt.Errorf("openaicompat: no models configured")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
