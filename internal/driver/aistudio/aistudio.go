// Package aistudio implements the Google AI Studio (Gemini API) channel —
// the official generativelanguage.googleapis.com v1beta plane with plain
// API-key auth (BP-326). No client disguise is needed: the API takes an
// x-goog-api-key header and answers plain curl.
//
// The free tier serves gemini-4-hydrogen only; Carbon/Argon are paid-only
// even though ListModels still lists them, so the servable set is the fixed
// allow-list in servedModels and the catalog is never echoed as routable
// (目录≠可调). Free-tier traffic is used by Google for product improvement —
// the panel guide carries the warning.
//
// Wire notes (measured 2026-11-14, 4 live keys):
//   - streaming is :streamGenerateContent?alt=sse, one JSON frame per
//     `data:` line, unwrapped (no CloudCode "response" envelope);
//   - thought parts carry thought:true, and functionCall parts may carry a
//     thoughtSignature (same shape the antigravity plane uses);
//   - the model can open a 200 SSE stream and then report an error frame —
//     mid-stream frames go through the same HTTP classifier;
//   - capacity pressure shows up as 503 UNAVAILABLE or a hung connection
//     with zero bytes; both are upstream busy, not bad keys.
package aistudio

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/streamx"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// servedModels is the fixed expose list — the user ruling for BP-326 is
// hydrogen only (flash families and paid models are out of scope).
var servedModels = []string{"gemini-4-hydrogen"}

// Creds is the persisted credential shape for an aistudio account.
type Creds struct {
	APIKey string `json:"api_key"`
}

// KeyFromCreds extracts the API key from an account's stored credentials.
func KeyFromCreds(acc *model.Account) (string, error) {
	var c Creds
	if len(acc.CredsJSON) == 0 {
		return "", fmt.Errorf("aistudio: account %s has no credentials", acc.ID)
	}
	if err := json.Unmarshal(acc.CredsJSON, &c); err != nil {
		return "", fmt.Errorf("aistudio: account %s creds: %w", acc.ID, err)
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return "", fmt.Errorf("aistudio: account %s has no api_key", acc.ID)
	}
	return strings.TrimSpace(c.APIKey), nil
}

// Driver implements driver.Driver for the aistudio channel.
type Driver struct {
	cfg  config.AIStudioSettings
	http *http.Client // streaming chat calls (watchdog-configured)
	rpc  *http.Client // short ListModels calls
}

// New builds the driver.
func New(cfg config.AIStudioSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	return &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient()}
}

// Channel returns the channel identifier.
func (d *Driver) Channel() string { return model.ChannelAIStudio }

func (d *Driver) baseURL() string { return strings.TrimRight(d.cfg.BaseURL, "/") }

// ——— request shapes (build side) ———

type geminiBlob struct {
	MIMEType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type reqFuncCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type reqFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type reqPart struct {
	Text             string       `json:"text,omitempty"`
	Thought          bool         `json:"thought,omitempty"`
	ThoughtSignature string       `json:"thoughtSignature,omitempty"`
	InlineData       *geminiBlob  `json:"inlineData,omitempty"`
	FunctionCall     *reqFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *reqFuncResp `json:"functionResponse,omitempty"`
}

type reqContent struct {
	Role  string    `json:"role"`
	Parts []reqPart `json:"parts"`
}

func textOf(parts []ir.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func toReqParts(parts []ir.ContentPart) []reqPart {
	var out []reqPart
	for _, p := range parts {
		switch p.Kind {
		case "text":
			if p.Text != "" {
				out = append(out, reqPart{Text: p.Text})
			}
		case "image":
			if p.ImageB64 != "" {
				out = append(out, reqPart{InlineData: &geminiBlob{MIMEType: p.ImageMIME, Data: p.ImageB64}})
			}
		}
	}
	return out
}

// buildContents maps the normalized conversation onto Gemini contents.
// History thinking replays ONLY when the upstream signed it — an unsigned
// thought part is a guess this API never asked for (BP-69 vocabulary).
func buildContents(req *ir.ChatRequest) (contents []reqContent, sysParts []reqPart) {
	for i := range req.Messages {
		msg := &req.Messages[i]
		switch msg.Role {
		case ir.RoleSystem:
			if t := textOf(msg.Content); t != "" {
				sysParts = append(sysParts, reqPart{Text: t})
			}
		case ir.RoleUser:
			if parts := toReqParts(msg.Content); len(parts) > 0 {
				contents = append(contents, reqContent{Role: "user", Parts: parts})
			}
		case ir.RoleAssistant:
			var parts []reqPart
			for _, th := range msg.Thinking {
				if th.Text != "" && th.Signature != "" {
					parts = append(parts, reqPart{Text: th.Text, Thought: true, ThoughtSignature: th.Signature})
				}
			}
			if t := textOf(msg.Content); t != "" {
				parts = append(parts, reqPart{Text: t})
			}
			for _, tc := range msg.ToolCalls {
				args := map[string]any{}
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
				parts = append(parts, reqPart{FunctionCall: &reqFuncCall{Name: tc.Name, Args: args}})
			}
			if len(parts) > 0 {
				contents = append(contents, reqContent{Role: "model", Parts: parts})
			}
		case ir.RoleTool:
			var payload map[string]any
			if !msg.IsError && len(msg.Content) > 0 && msg.Content[0].Kind == "text" {
				_ = json.Unmarshal([]byte(msg.Content[0].Text), &payload)
			}
			if msg.IsError {
				// BP-224 family: a failed tool result rides
				// functionResponse.response.error — leaving it in the success
				// shape makes the model plan on a result that never happened.
				payload = map[string]any{"error": textOf(msg.Content)}
			}
			if payload == nil {
				payload = map[string]any{"content": textOf(msg.Content)}
			}
			name := msg.ToolName
			if name == "" {
				name = "tool"
			}
			parts := []reqPart{{FunctionResponse: &reqFuncResp{Name: name, Response: payload}}}
			// Tool-result images ride as SIBLING inlineData parts: Google
			// rejects a text field inside function_response.parts ("Unknown
			// name" — live probe 2026-11-12, BP-315 note), and the official
			// plane accepts inline_data beside it.
			for _, p := range msg.Content {
				if p.Kind == "image" && p.ImageB64 != "" {
					parts = append(parts, reqPart{InlineData: &geminiBlob{MIMEType: p.ImageMIME, Data: p.ImageB64}})
				}
			}
			contents = append(contents, reqContent{Role: "user", Parts: parts})
		}
	}
	return contents, sysParts
}

// buildPayload assembles the generateContent request body.
func buildPayload(req *ir.ChatRequest, upstreamModel string) ([]byte, error) {
	contents, sysParts := buildContents(req)
	if len(contents) == 0 {
		return nil, fmt.Errorf("aistudio: no content to send")
	}
	inner := map[string]any{"contents": contents}
	if len(sysParts) > 0 {
		inner["systemInstruction"] = map[string]any{"parts": sysParts}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, td := range req.Tools {
			decl := map[string]any{"name": td.Name}
			if td.Description != "" {
				decl["description"] = td.Description
			}
			if td.Parameters != nil {
				if s := sanitizeSchema(td.Parameters); s != nil {
					decl["parameters"] = s
				}
			}
			decls = append(decls, decl)
		}
		inner["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	choice, name, _ := ir.NormalizeToolChoice(req.ToolChoice)
	if choice != nil {
		fc := map[string]any{}
		switch choice {
		case "none":
			fc["mode"] = "NONE"
		case "required":
			fc["mode"] = "ANY"
			if name != "" {
				fc["allowedFunctionNames"] = []string{name}
			}
		case "auto":
			fc["mode"] = "AUTO"
		}
		if len(fc) > 0 {
			inner["toolConfig"] = map[string]any{"functionCallingConfig": fc}
		}
	}
	genConfig := map[string]any{}
	if req.MaxTokens > 0 {
		genConfig["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		genConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		genConfig["topP"] = *req.TopP
	}
	if req.TopK != nil {
		genConfig["topK"] = *req.TopK
	}
	if len(req.Stop) > 0 {
		genConfig["stopSequences"] = req.Stop
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		// Gemini 3.x/4.x are level-based; a client budget converts to the
		// nearest band (same conversion the antigravity plane runs).
		level := req.Thinking.Level
		if level == "" {
			level = ir.LevelFromBudget(req.Thinking.BudgetTokens)
		}
		// includeThoughts rides ALWAYS when thinking is on (antigravity plane
		// precedent): the camelCase key only, or a 400 "_include_thoughts is
		// already set" is possible upstream (BP-41 note).
		thinking := map[string]any{"includeThoughts": true}
		if level != "" {
			thinking["thinkingLevel"] = strings.ToUpper(level)
		}
		genConfig["thinkingConfig"] = thinking
	} else if req.RequestedEffort != "" {
		genConfig["thinkingConfig"] = map[string]any{
			"thinkingLevel":   strings.ToUpper(req.RequestedEffort),
			"includeThoughts": true,
		}
	}
	if len(genConfig) > 0 {
		inner["generationConfig"] = genConfig
	}
	return json.Marshal(inner)
}

// ——— response shapes (parse side) ———

type frameCandidate struct {
	Content struct {
		Parts []struct {
			Text         string `json:"text"`
			Thought      bool   `json:"thought"`
			FunctionCall *struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			} `json:"functionCall"`
		} `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

type frameUsage struct {
	PromptTokenCount        int  `json:"promptTokenCount"`
	CandidatesTokenCount    int  `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int  `json:"thoughtsTokenCount"`
	CachedContentTokenCount *int `json:"cachedContentTokenCount"`
}

// mapFinish shares the antigravity plane's refusal vocabulary (BP-272): the
// vendor's reasons beyond STOP/MAX_TOKENS are refusals, not quiet successes.
func mapFinish(reason string) ir.StopReason {
	switch strings.ToUpper(reason) {
	case "STOP":
		return ir.StopEnd
	case "MAX_TOKENS":
		return ir.StopLength
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST",
		"SPII", "IMAGE_SAFETY", "MALFORMED_RESPONSE":
		return ir.StopContentFilter
	default:
		return ir.StopEnd
	}
}

// classifyHTTP maps a non-200 response (or a mid-stream error frame) into the
// normalized error taxonomy. 429 with a RetryInfo detail is an upstream-stated
// window → ClassModelRate + ResetHard (BP-135 vocabulary); without one it is
// generic upstream busy (BP-314: zero-reset 429 must not hold the account for
// a guessed window).
func classifyHTTP(status int, body, upstreamModel string) *uperr.Error {
	var e struct {
		Error struct {
			Code    int              `json:"code"`
			Message string           `json:"message"`
			Status  string           `json:"status"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(body)
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	switch status {
	case http.StatusBadRequest:
		if strings.Contains(msg, "API key not valid") || strings.Contains(e.Error.Status, "API_KEY_INVALID") {
			return uperr.New(uperr.ClassAuth, status, "aistudio: %s", msg)
		}
		return uperr.New(uperr.ClassBadRequest, status, "aistudio: %s", msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return uperr.New(uperr.ClassAuth, status, "aistudio: %s", msg)
	case http.StatusNotFound:
		return uperr.New(uperr.ClassNotFound, status, "aistudio: %s", msg)
	case http.StatusTooManyRequests:
		if delay, ok := retryDelay(e.Error.Details); ok {
			ue := uperr.New(uperr.ClassModelRate, status, "aistudio: rate limited: %s", msg)
			ue.ResetAt = time.Now().Add(delay)
			ue.ResetHard = true
			ue.Model = upstreamModel
			return ue
		}
		ue := uperr.New(uperr.ClassUpstreamBusy, status, "aistudio: rate limited (no reset stated): %s", msg)
		ue.Model = upstreamModel
		return ue
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return uperr.New(uperr.ClassUpstreamBusy, status, "aistudio: %s", msg)
	default:
		return uperr.New(uperr.ClassTransport, status, "aistudio: http %d: %s", status, msg)
	}
}

// retryDelay digs the google.rpc.RetryInfo delay out of an error body.
func retryDelay(details []map[string]any) (time.Duration, bool) {
	for _, d := range details {
		if t, _ := d["@type"].(string); !strings.Contains(t, "RetryInfo") {
			continue
		}
		if s, _ := d["retryDelay"].(string); s != "" {
			if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
				return dur, true
			}
		}
	}
	return 0, false
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ——— driver.Driver ———

// Stream performs the upstream call, emitting IR events.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	key, err := KeyFromCreds(call.Account)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	payload, err := buildPayload(call.Request, call.UpstreamModel)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 0, "aistudio: build payload: %v", err)
	}
	url := d.baseURL() + "/models/" + call.UpstreamModel + ":streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 0, "aistudio: request: %v", err)
	}
	httpReq.Header.Set("x-goog-api-key", key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := d.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "aistudio: canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "aistudio: post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		ue := classifyHTTP(resp.StatusCode, string(body), call.UpstreamModel)
		httpx.ApplyRetryAfter(ue, resp.Header)
		return nil, ue
	}

	out := &ir.ChatResponse{Model: call.UpstreamModel}
	var (
		textBuf, thinkBuf strings.Builder
		toolCalls         []ir.ToolCall
		usage             ir.Usage
		gotUsage, term    bool
		stop              ir.StopReason
	)
	// maxSSELine guards against unbounded SSE lines (a frame carrying a fat
	// functionCall must not split — same ceiling the antigravity plane uses).
	const maxSSELine = 8 << 20
	sc := streamx.NewSplitScanner(resp.Body) // BP-318: 粘帧 data 行在行级拆成多次 Scan
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "aistudio: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		frameBody := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if frameBody == "" || frameBody == "[DONE]" {
			continue
		}
		var frame struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
			Candidates     []frameCandidate `json:"candidates"`
			UsageMetadata  *frameUsage      `json:"usageMetadata"`
			PromptFeedback *struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
		}
		if err := json.Unmarshal([]byte(frameBody), &frame); err != nil {
			continue // tolerate unknown frames
		}
		if frame.Error != nil {
			// Mid-stream error frame on a 200 stream: classify with the
			// frame's own code so quota/capacity land in the right bucket.
			return nil, classifyHTTP(frame.Error.Code, frameBody, call.UpstreamModel)
		}
		if pf := frame.PromptFeedback; pf != nil && pf.BlockReason != "" &&
			!strings.EqualFold(pf.BlockReason, "BLOCK_REASON_UNSPECIFIED") {
			return nil, uperr.New(uperr.ClassContentBlocked, 400,
				"aistudio: prompt blocked by safety filter (%s)", pf.BlockReason)
		}
		for _, cand := range frame.Candidates {
			for _, part := range cand.Content.Parts {
				switch {
				case part.Text != "" && part.Thought:
					thinkBuf.WriteString(part.Text)
					if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: part.Text}); err != nil {
						return nil, err
					}
				case part.Text != "":
					textBuf.WriteString(part.Text)
					if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: part.Text}); err != nil {
						return nil, err
					}
				case part.FunctionCall != nil:
					args, _ := json.Marshal(part.FunctionCall.Args)
					tc := ir.ToolCall{
						ID:        fmt.Sprintf("call_%d_%s", len(toolCalls), randHex(4)),
						Name:      part.FunctionCall.Name,
						Arguments: string(args),
					}
					toolCalls = append(toolCalls, tc)
					if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: len(toolCalls) - 1, ToolCall: &ir.ToolCall{ID: tc.ID, Name: tc.Name}}); err != nil {
						return nil, err
					}
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: len(toolCalls) - 1, Delta: tc.Arguments}); err != nil {
						return nil, err
					}
				}
			}
			if cand.FinishReason != "" {
				term = true
				// BP-154 family: MALFORMED_FUNCTION_CALL is a sampling glitch,
				// not a valid end — a clean 200 with nothing in it would eat
				// the retry. Only while nothing reached the client.
				if strings.EqualFold(cand.FinishReason, "MALFORMED_FUNCTION_CALL") &&
					textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolCalls) == 0 {
					return nil, uperr.New(uperr.ClassTransport, 200,
						"aistudio: upstream returned a malformed function call (finishReason=MALFORMED_FUNCTION_CALL)")
				}
				stop = mapFinish(cand.FinishReason)
			}
		}
		if u := frame.UsageMetadata; u != nil {
			usage = ir.Usage{
				InputTokens:     u.PromptTokenCount,
				OutputTokens:    u.CandidatesTokenCount,
				ReasoningTokens: u.ThoughtsTokenCount,
			}
			if u.CachedContentTokenCount != nil {
				usage.CachedInputTokens = *u.CachedContentTokenCount
				usage.CacheKnown = true
			}
			gotUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(resp.Body) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"aistudio: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "aistudio: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "aistudio: stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "aistudio: stream canceled: %v", ctx.Err())
	}
	if !term {
		return nil, uperr.New(uperr.ClassTransport, 0, "aistudio: stream ended without terminal signal")
	}
	// BP-298: read the tail so the pooled connection survives.
	httpx.Drain(resp.Body)

	if len(toolCalls) > 0 && (stop == "" || stop == ir.StopEnd) {
		// Gemini reports finishReason STOP even when it emitted function
		// calls; from the client's perspective this turn ends in tool use.
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.Message.ToolCalls = toolCalls
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

// Models returns the servable set. It is deliberately the fixed allow-list,
// not the per-key catalog: the upstream lists paid-only models too, and
// advertising them invites calls that can only fail (目录≠可调).
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	out := make([]string, len(servedModels))
	copy(out, servedModels)
	return out, nil
}

// ——— driver.Prober ———

// listModels fetches the account's catalog (the liveness signal).
func (d *Driver) listModels(ctx context.Context, key string) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL()+"/models?pageSize=1000", nil)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 0, "aistudio: request: %v", err)
	}
	httpReq.Header.Set("x-goog-api-key", key)
	resp, err := d.rpc.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "aistudio: canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "aistudio: get models: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, string(body), "")
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, uperr.New(uperr.ClassTransport, resp.StatusCode, "aistudio: decode models: %v", err)
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, strings.TrimPrefix(m.Name, "models/"))
	}
	return names, nil
}

// Probe refreshes account state: a ListModels 200 proves the key is alive.
// The API key has no billing endpoint (the free tier has no balance to read),
// so there are no windows or credits to report — the panel shows the note.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	key, err := KeyFromCreds(acc)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	names, err := d.listModels(ctx, key)
	if err != nil {
		return nil, err
	}
	note := fmt.Sprintf("ok: key alive, %d models in catalog", len(names))
	for _, n := range names {
		if n == servedModels[0] {
			note = fmt.Sprintf("ok: key alive, %s in catalog (%d models)", servedModels[0], len(names))
			break
		}
	}
	return &driver.ProbeResult{Note: note}, nil
}
