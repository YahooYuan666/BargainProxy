// Package antigravity implements the Google Cloud Code Assist channel
// (Antigravity reverse proxy). Protocol notes follow the clean-room behaviour
// spec recorded in research/2026-10-13-三项目与生态调研.md: v1internal
// generateContent wrapper, loadCodeAssist project discovery, quota window
// buckets, QuotaFailure reset parsing, force-stream aggregation.
package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

const (
	// refreshSkew refreshes credentials this far before expiry (50 minutes).
	refreshSkew = 50 * time.Minute
	// maxSSELine guards against unbounded SSE lines.
	maxSSELine = 8 << 20
	// skipThoughtSignature is Google's documented placeholder for model-turn
	// functionCall parts whose original thought_signature was not preserved.
	// Gemini 3 thinking models stamp every functionCall part with a signature
	// and reject replayed tool-call history without one (HTTP 400 "Function
	// call is missing a thought_signature in functionCall parts"). OpenAI /
	// Anthropic-protocol clients cannot round-trip the Gemini-internal
	// signature, so the replayed part carries this sentinel instead —
	// AIClient2API OpenAIConverter.GEMINI_OPENAI_THOUGHT_SIGNATURE parity
	// (用户实测 2026-10-16：gemini-3.8-flash-high + 工具调用复现 400)。
	skipThoughtSignature = "skip_thought_signature_validator"
)

// Creds is the on-disk credential shape for an antigravity account.
type Creds struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	ProjectID    string    `json:"project_id,omitempty"`
	Email        string    `json:"email,omitempty"`
}

// Driver implements driver.Driver for the antigravity channel.
type Driver struct {
	cfg  config.AntigravitySettings
	http *http.Client // streaming chat calls (no total timeout + header/idle watchdogs)
	rpc  *http.Client // short refresh / quota / project calls (total timeout)

	// Experimental real-thought-signature replay (BP-44): capture upstream
	// signatures and attach them when the tool-call history is replayed.
	// Off by default; toggled from the panel settings page.
	sigCache  *thoughtSigCache
	sigReplay atomic.Bool
}

// New builds the driver. httpClient may be nil (default client).
func New(cfg config.AntigravitySettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient() // streaming: no total timeout, header watchdog
	}
	return &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient(), sigCache: newThoughtSigCache()}
}

// SetThoughtSignatureReplay toggles the experimental real-signature replay
// (panel hot setting; gateway asserts this method). Off = the BP-42
// placeholder path, unchanged.
func (d *Driver) SetThoughtSignatureReplay(enabled bool) { d.sigReplay.Store(enabled) }

// sigLookup builds the replay resolver passed to buildRequest: non-nil only
// while the experimental toggle is on (nil keeps the placeholder path). The
// returned flag reports whether any cached signature was actually attached —
// Stream uses it to self-heal a stale-signature rejection. The account is part
// of the cache key: signatures are account-bound upstream (sub2api field
// evidence), so a rotated account must miss rather than replay a doomed one.
func (d *Driver) sigLookup(upstreamModel, accountID string) (func(callID, name string) (string, bool), *bool) {
	used := new(bool)
	if !d.sigReplay.Load() {
		return nil, used
	}
	return func(callID, name string) (string, bool) {
		sig, ok := d.sigCache.get(accountID, upstreamModel, callID, name)
		if ok {
			*used = true
		}
		return sig, ok
	}, used
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelAntigravity }

// ParseCreds decodes the account credential blob.
func ParseCreds(raw []byte) (*Creds, error) {
	var c Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("antigravity: parse creds: %w", err)
	}
	return &c, nil
}

// Refresh implements driver.Refresher: exchanges the refresh token for a new
// access token and updates the credential blob in place.
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return err
	}
	if creds.RefreshToken == "" {
		return uperr.New(uperr.ClassAuth, 401, "antigravity: no refresh_token")
	}
	// Explicit config values win; otherwise fall back to Google's public CLI
	// client (same fallback the panel onboarding uses) so probe/refresh work
	// out of the box without config.json OAuth entries.
	clientID, clientSecret := d.cfg.GoogleOAuthClientID, d.cfg.GoogleOAuthClientSecret
	if clientID == "" || clientSecret == "" {
		clientID, clientSecret = config.GoogleOAuthFallback()
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {creds.RefreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "antigravity refresh: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		class := uperr.ClassAuth
		if resp.StatusCode >= 500 {
			class = uperr.ClassTransport
		}
		return uperr.New(class, resp.StatusCode, "antigravity refresh: %s", truncate(body, 300))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return uperr.New(uperr.ClassAuth, 0, "antigravity refresh: bad token response")
	}
	creds.AccessToken = tok.AccessToken
	if tok.ExpiresIn > 0 {
		creds.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		creds.Expiry = time.Now().Add(time.Hour)
	}
	updated, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	acc.CredsJSON = updated
	acc.CredsDirty = true
	return nil
}

// ensureFresh refreshes when the access token is about to expire.
func (d *Driver) ensureFresh(ctx context.Context, acc *model.Account) (*Creds, error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	if creds.AccessToken != "" && time.Until(creds.Expiry) > refreshSkew {
		return creds, nil
	}
	if err := d.Refresh(ctx, acc); err != nil {
		if ue, ok := uperr.As(err); ok && ue.Class == uperr.ClassTransport && creds.AccessToken != "" && time.Now().Before(creds.Expiry) {
			return creds, nil // still valid, keep going
		}
		return nil, err
	}
	return ParseCreds(acc.CredsJSON)
}

// Stream implements driver.Driver.
// resolveCreds returns fresh credentials and the effective project id for a
// request, discovering the project on first use. Shared by the chat and image
// paths; a discovery failure is not fatal (some upstream builds accept
// requests without a project).
func (d *Driver) resolveCreds(ctx context.Context, acc *model.Account) (*Creds, string, error) {
	creds, err := d.ensureFresh(ctx, acc)
	if err != nil {
		return nil, "", err
	}
	project := creds.ProjectID
	if project == "" {
		project = acc.ProjectID
	}
	if project == "" {
		if pid, perr := d.loadCodeAssist(ctx, acc.ID, creds.AccessToken); perr == nil && pid != "" {
			project = pid
			creds.ProjectID = pid
			if b, merr := json.Marshal(creds); merr == nil {
				acc.CredsJSON = b
				acc.CredsDirty = true
			}
		}
	}
	return creds, project, nil
}

func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	acc := call.Account
	creds, project, err := d.resolveCreds(ctx, acc)
	if err != nil {
		return nil, err
	}

	lookup, sigUsed := d.sigLookup(call.UpstreamModel, acc.ID)
	body, err := buildRequest(project, call.UpstreamModel, call.Request, lookup)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "antigravity: build payload: %v", err)
	}

	resp, err := d.doStream(ctx, creds.AccessToken, call.UpstreamModel, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// Keep the upstream error body in the server log: the classifier only
		// forwards a truncated message to the client, and clients (AI SDKs)
		// routinely swallow the detail — diagnosing a 400 then needs guessing
		// (user report 2026-10-16 14:11: two 400s with no server-side trace).
		log.Printf("antigravity: upstream error status=%d model=%s account=%s body=%s",
			resp.StatusCode, call.UpstreamModel, acc.ID, truncate(raw, 500))
		// Optional: write the request we actually sent beside the answer, so a
		// generic upstream rejection ("Request contains an invalid argument",
		// which names no field) can be diffed against a hand-built request that
		// answers 200. Off unless channels.antigravity.dump_dir is set.
		d.dumpFailure(acc.ID, call.UpstreamModel, resp.StatusCode, body, raw)
		ue := classifyHTTP(resp.StatusCode, raw, call.UpstreamModel)
		// Experimental replay (BP-44) can go stale — the account switched, the
		// model changed, or the upstream expired the signature. The experiment
		// must never make things worse than the placeholder path: on a
		// thought_signature rejection retry once with the placeholder.
		if !(*sigUsed && ue.Class == uperr.ClassBadRequest && strings.Contains(ue.Message, "thought_signature")) {
			return nil, ue
		}
		log.Printf("antigravity: replayed thought_signature rejected by upstream; retrying with placeholder")
		body, err = buildRequest(project, call.UpstreamModel, call.Request, nil)
		if err != nil {
			return nil, ue
		}
		resp, err = d.doStream(ctx, creds.AccessToken, call.UpstreamModel, body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, classifyHTTP(resp.StatusCode, raw, call.UpstreamModel)
		}
	}
	body2 := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer body2.Close()

	// Diagnosis aid (BP-115): with channels.antigravity.dump_ok set, also keep
	// the requests that WORKED, so a failing body can be diffed against a
	// passing one. Without this the dump only ever shows failures, and the
	// difference — the whole question — stays invisible.
	if d.cfg.DumpOK && d.cfg.DumpDir != "" {
		d.dumpFailure(acc.ID, call.UpstreamModel, resp.StatusCode, body, []byte("(200; stream not captured)"))
	}
	return d.parseSSE(ctx, body2, call, emit)
}

// doStream posts to the v1internal endpoint. Host order matches the
// AIClient2API reference (baseURLs[0] = daily): the daily host serves chat
// from a separate quota pool — pinned to prod, exhausted accounts 429 even
// while daily still has headroom (observed live 2026-10-15).
func (d *Driver) doStream(ctx context.Context, token, upstreamModel string, body []byte) (*http.Response, error) {
	ua := uaForModel(upstreamModel)
	hosts := []string{d.cfg.BaseURL}
	if d.cfg.DailyURL != "" && d.cfg.DailyURL != d.cfg.BaseURL {
		hosts = append([]string{d.cfg.DailyURL}, hosts...) // daily first, prod fallback
	}
	u := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", strings.TrimRight(hosts[0], "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := d.http.Do(req)
	if err != nil {
		// Transport error on the primary host: try the fallback host once.
		if len(hosts) > 1 {
			u2 := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", strings.TrimRight(hosts[1], "/"))
			req2, rerr := http.NewRequestWithContext(ctx, http.MethodPost, u2, bytes.NewReader(body))
			if rerr == nil {
				req2.Header = req.Header.Clone()
				if resp2, err2 := d.http.Do(req2); err2 == nil {
					return resp2, nil
				}
			}
		}
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "antigravity: %v", err)
	}
	if resp.StatusCode >= 500 && len(hosts) > 1 {
		// Server-side failure on primary: retry once on the fallback host.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		u2 := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", strings.TrimRight(hosts[1], "/"))
		req2, rerr := http.NewRequestWithContext(ctx, http.MethodPost, u2, bytes.NewReader(body))
		if rerr == nil {
			req2.Header.Set("Content-Type", "application/json")
			req2.Header.Set("Authorization", "Bearer "+token)
			req2.Header.Set("User-Agent", ua)
			req2.Header.Set("Accept", "text/event-stream")
			if resp2, err2 := d.http.Do(req2); err2 == nil {
				return resp2, nil
			}
		}
		// Fallback also failed: report the primary status.
		resp2 := &http.Response{
			StatusCode: resp.StatusCode,
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Header:     http.Header{},
		}
		return resp2, nil
	}
	return resp, nil
}

// parseSSE consumes the Gemini SSE stream, emitting IR events and building
// the aggregated response.
func (d *Driver) parseSSE(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)

	out := &ir.ChatResponse{
		Model:     call.Request.Model,
		CreatedAt: time.Now().Unix(),
		Message:   ir.Message{Role: ir.RoleAssistant},
	}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		toolCalls  []ir.ToolCall
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
		lastSig    string // newest thought_signature seen in this turn (BP-44)
	)

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "antigravity: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		// BP-180: a bare "[DONE]" without the "data:" prefix terminates too.
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
		// Mid-stream error frame on a 200 stream (ticket 36): Gemini opens the
		// stream and can then report quota/overload/safety errors as a frame;
		// ignoring it counted the request as success (no penalty, sticky
		// stayed). Classify via the HTTP classifier with a derived status.
		if fe := detectFrameError(payload, call.UpstreamModel); fe != nil {
			return nil, fe
		}
		var frame struct {
			Response geminiResponse `json:"response"`
			// Some builds return the response unwrapped.
			Candidates []struct {
				Content      geminiContent `json:"content"`
				FinishReason string        `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata  *geminiUsage    `json:"usageMetadata"`
			PromptFeedback *promptFeedback `json:"promptFeedback"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue // tolerate unknown frames
		}
		gr := frame.Response
		if len(gr.Candidates) == 0 && len(frame.Candidates) > 0 {
			gr.Candidates = nil // unwrapped form handled below
			for _, c := range frame.Candidates {
				gr.Candidates = append(gr.Candidates, geminiCandidate{Content: c.Content, FinishReason: c.FinishReason})
			}
			gr.UsageMetadata = frame.UsageMetadata
			if frame.PromptFeedback != nil {
				gr.PromptFeedback = frame.PromptFeedback
			}
		}
		// BP-155: a safety block arrives as promptFeedback.blockReason with NO
		// candidates and NO error object. The old path read the clean stream
		// end as "no terminal signal" (transport) and cross-account-retried a
		// request no account could answer. No account penalty, no rotation.
		if pf := gr.PromptFeedback; pf != nil && pf.BlockReason != "" &&
			!strings.EqualFold(pf.BlockReason, "BLOCK_REASON_UNSPECIFIED") {
			return nil, uperr.New(uperr.ClassContentBlocked, 400,
				"antigravity: prompt blocked by safety filter (%s): %s", pf.BlockReason, pf.BlockReasonMessage)
		}
		for _, cand := range gr.Candidates {
			for _, part := range cand.Content.Parts {
				// Track the newest signature in this turn: Gemini usually puts
				// it on the functionCall part itself, but a leading thought /
				// text part may carry it for the calls that follow (BP-44).
				if part.ThoughtSignature != "" {
					lastSig = part.ThoughtSignature
				}
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
					// Capture the real signature for the experimental replay
					// cache (BP-44). The call id we mint here is exactly what
					// the client echoes back next turn, so it is the join key.
					if sig := part.ThoughtSignature; sig != "" || lastSig != "" {
						if sig == "" {
							sig = lastSig
						}
						if d.sigReplay.Load() {
							d.sigCache.put(call.Account.ID, call.UpstreamModel, tc.ID, tc.Name, sig)
							log.Printf("antigravity: thought-signature replay: captured signature for call %s (model=%s, account=%s, %d chars)",
								tc.ID, call.UpstreamModel, call.Account.ID, len(sig))
						}
					}
					if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: len(toolCalls) - 1, ToolCall: &ir.ToolCall{ID: tc.ID, Name: tc.Name}}); err != nil {
						return nil, err
					}
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: len(toolCalls) - 1, Delta: tc.Arguments}); err != nil {
						return nil, err
					}
				}
			}
			if cand.FinishReason != "" {
				terminated = true
				// BP-154: MALFORMED_FUNCTION_CALL is an upstream sampling
				// glitch, not a valid end — mapping it to STOP used to hand
				// the client an empty 200 success with no retry chance
				// (sub2api #8362). Only while nothing has reached the client;
				// once frames flowed, ending cleanly is the least-bad exit.
				if strings.EqualFold(cand.FinishReason, "MALFORMED_FUNCTION_CALL") &&
					textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(toolCalls) == 0 {
					return nil, uperr.New(uperr.ClassTransport, 200,
						"antigravity: upstream returned a malformed function call (finishReason=MALFORMED_FUNCTION_CALL)")
				}
				stop = mapFinish(cand.FinishReason)
			}
		}
		if gr.UsageMetadata != nil {
			u := gr.UsageMetadata
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
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"antigravity: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "antigravity: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "antigravity: stream read: %v", err)
	}

	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "antigravity: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "antigravity: stream ended without terminal signal")
	}

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

// loadCodeAssist discovers the Cloud AI Companion project id. The project id
// feeds request construction, so a truncated read is an error, not a miss.
func (d *Driver) loadCodeAssist(ctx context.Context, accID, token string) (string, error) {
	u := strings.TrimRight(d.cfg.BaseURL, "/") + "/v1internal:loadCodeAssist"
	body := []byte(`{"metadata":{"pluginType":"GEMINI"}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("antigravity: loadCodeAssist read failed (account=%s): %v", accID, err)
		return "", fmt.Errorf("loadCodeAssist: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("loadCodeAssist: %s", truncate(raw, 200))
	}
	var out struct {
		CloudAICompanionProject string `json:"cloudaicompanionProject"`
		Project                 string `json:"project"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.CloudAICompanionProject != "" {
		return out.CloudAICompanionProject, nil
	}
	return out.Project, nil
}

// Probe implements driver.Prober: refresh credentials and fetch quota windows
// via retrieveUserQuotaSummary (best effort).
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	creds, err := d.ensureFresh(ctx, acc)
	if err != nil {
		return nil, err
	}
	res := &driver.ProbeResult{Note: "credentials refreshed"}
	windows, err := d.fetchQuota(ctx, acc.ID, creds.AccessToken)
	if err != nil {
		log.Printf("antigravity: probe quota fetch failed (account=%s): %v", acc.ID, err)
		res.Note += "; quota fetch failed: " + err.Error()
	} else if len(windows) > 0 {
		res.Windows = windows
		res.Note = "credentials + quota windows refreshed"
	} else {
		res.Note += "; quota returned no windows"
	}
	return res, nil
}

// fetchQuota reads quota summary buckets. The upstream serves the summary
// from the DAILY host first (AIClient2API parity: getQuotaSummary pins
// daily-cloudcode-pa); the prod host is tried as fallback. Windows feed pool
// scoring, so a truncated read must surface as an error rather than an empty
// window map (ticket 11).
func (d *Driver) fetchQuota(ctx context.Context, accID, token string) (map[string]model.Window, error) {
	hosts := []string{d.cfg.BaseURL}
	if d.cfg.DailyURL != "" && d.cfg.DailyURL != d.cfg.BaseURL {
		hosts = append([]string{d.cfg.DailyURL}, hosts...) // daily first
	}
	var lastErr error
	for _, host := range hosts {
		windows, err := d.fetchQuotaHost(ctx, host, token)
		if err == nil {
			return windows, nil
		}
		lastErr = err
		log.Printf("antigravity: quota probe failed on %s (account=%s): %v", host, accID, err)
	}
	return nil, lastErr
}

// antigravityUA is the CLI identity the Antigravity endpoints expect. A bare
// or generic User-Agent gets 429-limited on chat and 403 on quota (both
// observed live); the reference clients always send the full CLI string.
const antigravityUA = "antigravity/2.8.1 windows/amd64"

// argonCLIUA is the identity the Argon band requires (BP-162): requests for
// gemini-4-argon-* must come from the CLI fingerprint, not the IDE one.
const argonCLIUA = "antigravity/cli/1.4.2 darwin/arm64"

// uaForModel picks the request identity for a model.
func uaForModel(upstreamModel string) string {
	if strings.HasPrefix(upstreamModelName(upstreamModel), "gemini-4-argon") {
		return argonCLIUA
	}
	return antigravityUA
}

func (d *Driver) fetchQuotaHost(ctx context.Context, host, token string) (map[string]model.Window, error) {
	u := strings.TrimRight(host, "/") + "/v1internal:retrieveUserQuotaSummary"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// The quota endpoint rejects generic UAs with 403: it expects the CLI
	// identity (3r-waterline parity: "antigravity/<ver> <os>/<arch>").
	req.Header.Set("User-Agent", antigravityUA)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		log.Printf("antigravity: quota probe read failed (account host=%s): %v", host, err)
		return nil, fmt.Errorf("quota: read body: %w", err)
	}
	return ParseQuotaWindows(raw)
}

// ParseQuotaWindows extracts 5h/weekly windows from a quota summary body.
// Tolerant to the observed bucket shapes: groups[].buckets[] with bucketId
// containing "gemini" or "claude"/"3p" and "5h"/"weekly", plus
// remainingFraction / resetTime. Each account carries TWO ledgers — Gemini
// (gemini-*) and Claude/3p (claude-*) — each with a 5-hour and a weekly
// window (3r-waterline domain model). Keys: five_hour/weekly (Gemini ledger)
// and claude_five_hour/claude_weekly (Claude ledger).
func ParseQuotaWindows(raw []byte) (map[string]model.Window, error) {
	var doc struct {
		Groups []struct {
			Buckets []struct {
				BucketID          string  `json:"bucketId"`
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	now := time.Now()
	out := map[string]model.Window{}
	for _, g := range doc.Groups {
		for _, b := range g.Buckets {
			id := strings.ToLower(b.BucketID)
			isGemini := strings.Contains(id, "gemini")
			isClaude := strings.Contains(id, "claude") || strings.Contains(id, "3p")
			if !isGemini && !isClaude {
				continue
			}
			reset, err := time.Parse(time.RFC3339, b.ResetTime)
			if err != nil {
				continue
			}
			fr := b.RemainingFraction
			if fr < 0 {
				fr = 0
			}
			if fr > 1 {
				fr = 1
			}
			var key string
			switch {
			case strings.Contains(id, "5h") || strings.Contains(id, "5-hour"):
				if isClaude {
					key = "claude_five_hour"
				} else {
					key = "five_hour"
				}
			case strings.Contains(id, "weekly") || strings.Contains(id, "week"):
				if isClaude {
					key = "claude_weekly"
				} else {
					key = "weekly"
				}
			}
			if key == "" {
				continue
			}
			out[key] = model.Window{RemainingFraction: fr, ResetAt: reset, UpdatedAt: now}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("quota: no gemini/claude windows found")
	}
	return out, nil
}

// Models returns a static best-effort model list; the canonical catalog is
// configured in config.Models and the panel resolves discovery per account.
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return []string{
		"gemini-pro-agent",
		"gemini-3.1-pro-preview",
		"gemini-3-pro-high",
		"gemini-3-pro-low",
		"gemini-3.5-flash-high",
		"gemini-3.5-flash-low",
	}, nil
}

// --- payload building ---

type geminiPart struct {
	Text         string          `json:"text,omitempty"`
	Thought      bool            `json:"thought,omitempty"`
	InlineData   *geminiBlob     `json:"inlineData,omitempty"`
	FunctionCall *geminiFnCall   `json:"functionCall,omitempty"`
	FunctionResp *geminiFnResult `json:"functionResponse,omitempty"`
	// ThoughtSignature: Gemini 3 thinking models stamp functionCall parts with
	// a signature and REQUIRE it back when the tool-call history is replayed.
	// OpenAI-protocol clients cannot round-trip it, so replayed calls carry
	// Google's documented placeholder (see skipThoughtSignature).
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type geminiBlob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFnCall struct {
	// ID is Antigravity's PRIVATE extension on functionCall parts (not part of
	// the public Gemini API). Google's backend translates these Gemini-shaped
	// contents into Anthropic messages for Claude models and fills
	// tool_use.id from it; a missing id fails that translation with 400
	// "messages.N.content.M.tool_use.id: Field required" (BP-114, field
	// 2026-10-25 18:49 — the first claude-model turns with a tool history all
	// died there). The reference carries the same field and synthesizes ids
	// for clients that cannot know about it.
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFnResult struct {
	// ID must pair the functionCall it answers (see geminiFnCall.ID).
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	// Pointer: the upstream omits the field when no prefix cache was involved,
	// and "absent" must not read as a reported zero (BP-53 hit-rate sample).
	CachedContentTokenCount *int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int  `json:"thoughtsTokenCount"`
}

type geminiResponse struct {
	Candidates     []geminiCandidate `json:"candidates"`
	UsageMetadata  *geminiUsage      `json:"usageMetadata"`
	PromptFeedback *promptFeedback   `json:"promptFeedback"`
}

// promptFeedback is Google's pre-candidate safety verdict (BP-155).
type promptFeedback struct {
	BlockReason        string `json:"blockReason"`
	BlockReasonMessage string `json:"blockReasonMessage"`
}

type v1InternalRequest struct {
	Project     string          `json:"project,omitempty"`
	Model       string          `json:"model"`
	RequestType string          `json:"requestType"`
	UserAgent   string          `json:"userAgent"`
	RequestID   string          `json:"requestId"`
	Request     json.RawMessage `json:"request"`
}

// ensureAntigravityToolIDs backfills the private id field on functionCall /
// functionResponse parts (BP-114). Google's Gemini→Anthropic translation for
// Claude models requires both: tool_use.id comes from functionCall.id and
// tool_result.tool_use_id from functionResponse.id; either missing fails the
// whole request with 400 "messages.N.content.M.tool_use.id: Field required".
//
// Call ids come from the client history and are present in the normal flow;
// the pass exists for histories that lost them (repaired pairings, foreign
// replays). A call without an id gets a synthetic one; a result without an id
// is paired to the most recent unanswered call OF THE SAME NAME (FIFO) — the
// same closed-form strategy the reference uses, no cross-request state.
func ensureAntigravityToolIDs(contents []geminiContent) {
	// Pass 1: every call carries an id (synthetic when the history lost it).
	for ci := range contents {
		for pi := range contents[ci].Parts {
			if fc := contents[ci].Parts[pi].FunctionCall; fc != nil && fc.ID == "" {
				fc.ID = "toolu_vrtx_" + randHex(24)
			}
		}
	}
	// Pass 2: collect each name's call ids in wire order, then hand them out
	// FIFO to that name's id-less results. Parallel calls of one name pair in
	// appearance order — the only ordering information the wire carries.
	queue := map[string][]string{}
	for ci := range contents {
		for pi := range contents[ci].Parts {
			if fc := contents[ci].Parts[pi].FunctionCall; fc != nil && fc.Name != "" {
				queue[fc.Name] = append(queue[fc.Name], fc.ID)
			}
		}
	}
	for ci := range contents {
		for pi := range contents[ci].Parts {
			fr := contents[ci].Parts[pi].FunctionResp
			if fr == nil || fr.ID != "" || fr.Name == "" {
				continue
			}
			if ids := queue[fr.Name]; len(ids) > 0 {
				fr.ID = ids[0]
				queue[fr.Name] = ids[1:]
			}
		}
	}
}

// upstreamModelName translates a routed model id into the name the Antigravity
// upstream actually serves (BP-113).
// Field evidence 2026-10-25 17:17: the routes carried `gemini-claude-sonnet-4-6`
// — a CLIENT display name borrowed from the reference project — straight onto
// the wire. The upstream answered daily 404 "Requested entity was not found"
// and prod 429 RESOURCE_EXHAUSTED for that name, while the SAME account, the
// SAME payload and the bare name `claude-sonnet-4-6` answered 200 on the daily
// host (probed across all three accounts). The accounts were never dead and
// the quota never exhausted (the IDE shows 97% remaining): only the name was
// wrong, and the two error shapes were both lies about it.
//
// The reference resolves client display names to upstream ids before sending
// (resolveAntigravityUpstreamModel: gemini-claude-* → claude-*); this driver
// never had that layer, and it worked from 10-16 to 10-24 only because the
// daily host tolerated unknown names until Google tightened it.
func upstreamModelName(model string) string {
	if after, ok := strings.CutPrefix(model, "gemini-claude-"); ok {
		return "claude-" + after
	}
	// BP-162: the Argon band only answers through the CLI/App client and is
	// served as "-medium" upstream (AIClient2API e272a83); the IDE client
	// never sees it.
	if strings.HasPrefix(model, "gemini-4-argon") {
		return "gemini-4-argon-medium"
	}
	return model
}

// buildRequest assembles the v1internal payload. replay (may be nil) resolves
// a real thought_signature for a replayed tool call — nil or a miss falls back
// to the documented placeholder (BP-42/BP-44).
func buildRequest(project, upstreamModel string, req *ir.ChatRequest, replay func(callID, name string) (string, bool)) ([]byte, error) {
	upstreamModel = upstreamModelName(upstreamModel)
	var (
		contents   []geminiContent
		sysParts   []geminiPart
		tools      []map[string]any
		genConfig  = map[string]any{}
		toolConfig map[string]any
	)
	for _, msg := range req.Messages {
		switch msg.Role {
		case ir.RoleSystem:
			for _, p := range msg.Content {
				if p.Kind == "text" && p.Text != "" {
					sysParts = append(sysParts, geminiPart{Text: p.Text})
				}
			}
		case ir.RoleUser:
			c := geminiContent{Role: "user"}
			c.Parts = append(c.Parts, toGeminiParts(msg.Content)...)
			if len(c.Parts) > 0 {
				contents = append(contents, c)
			}
		case ir.RoleAssistant:
			c := geminiContent{Role: "model"}
			c.Parts = append(c.Parts, toGeminiParts(msg.Content)...)
			for _, tc := range msg.ToolCalls {
				var args map[string]any
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
				if args == nil {
					args = map[string]any{}
				}
				part := geminiPart{FunctionCall: &geminiFnCall{ID: tc.ID, Name: tc.Name, Args: args}}
				// Real signature when the experimental replay cache has one
				// (BP-44); placeholder otherwise (never nothing — the upstream
				// rejects a missing signature, BP-42).
				if replay != nil {
					if sig, ok := replay(tc.ID, tc.Name); ok {
						part.ThoughtSignature = sig
						log.Printf("antigravity: thought-signature replay: attached cached signature for call %s (%d chars)", tc.ID, len(sig))
					} else {
						part.ThoughtSignature = skipThoughtSignature
					}
				} else {
					part.ThoughtSignature = skipThoughtSignature
				}
				c.Parts = append(c.Parts, part)
			}
			if len(c.Parts) > 0 {
				contents = append(contents, c)
			}
		case ir.RoleTool:
			var payload map[string]any
			if !msg.IsError && len(msg.Content) > 0 && msg.Content[0].Kind == "text" {
				_ = json.Unmarshal([]byte(msg.Content[0].Text), &payload)
			}
			if msg.IsError {
				// BP-224: a failed tool result rides functionResponse's
				// response.error — leave it in the success payload shape and
				// the model reads the failure text as a real result and plans
				// on it (CLIProxyAPI 068fe153/81de4b9c #6838: the fix that
				// stopped dead-end loops on Bash errors).
				payload = map[string]any{"error": textOf(msg.Content)}
			}
			if payload == nil {
				payload = map[string]any{"content": textOf(msg.Content)}
			}
			name := msg.ToolName
			if name == "" {
				name = "tool"
			}
			// The result pairs its call by tool_call_id (BP-114): the upstream
			// translation fills tool_result.tool_use_id from this field, the
			// same way it fills tool_use.id on the call side. A result with no
			// id gets one from the synthetic pass below.
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{FunctionResp: &geminiFnResult{ID: msg.ToolCallID, Name: name, Response: payload}}},
			})
		}
	}
	ensureAntigravityToolIDs(contents)
	for _, t := range req.Tools {
		decl := map[string]any{"name": t.Name}
		if t.Description != "" {
			decl["description"] = t.Description
		}
		if t.Parameters != nil {
			// The endpoint accepts only the narrow Gemini schema subset;
			// OpenAI-style extras ($schema, additionalProperties, min/max…,
			// nested "schema" keys) trigger 400 Unknown name — reduce first.
			if cleaned := cleanGeminiSchema(t.Parameters); cleaned != nil {
				if m, ok := cleaned.(map[string]any); ok && len(m) > 0 {
					decl["parameters"] = m
				}
			}
		}
		tools = append(tools, map[string]any{"functionDeclarations": []map[string]any{decl}})
	}
	if len(tools) > 0 {
		mode := "AUTO"
		if s, ok := req.ToolChoice.(string); ok {
			switch strings.ToLower(s) {
			case "none":
				mode = "NONE"
			case "required", "any":
				mode = "ANY"
			}
		}
		toolConfig = map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
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
	if req.MaxTokens > 0 {
		genConfig["maxOutputTokens"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		genConfig["stopSequences"] = req.Stop
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		// includeThoughts is ALWAYS the camelCase key: the snake_case
		// include_thoughts addresses the same proto oneof and an upstream
		// level-config pass that adds the camelCase name makes Google reject
		// the request with 400 (_include_thoughts is already set) — the exact
		// failure AIClient2API fixed in 95e29ca. Only ever emit the camelCase
		// form, and only the surviving branch's key (budget XOR level).
		thinking := map[string]any{"includeThoughts": true}
		if strings.HasPrefix(upstreamModel, "claude-") {
			// Claude via antigravity bills thinking by budget; the level
			// vocabulary is not registered there. upstreamModel is already
			// normalized at the top of this function, so the bare name is
			// what arrives here (BP-113: the display form gemini-claude-*
			// became claude-* before the payload was assembled).
			if req.Thinking.BudgetTokens > 0 {
				thinking["thinkingBudget"] = req.Thinking.BudgetTokens
			} else if req.Thinking.Level != "" {
				thinking["thinkingLevel"] = strings.ToUpper(req.Thinking.Level)
			}
		} else {
			// Gemini 3.x family is level-based; a client budget converts to
			// the nearest band (AIClient2API deletes thinkingBudget for these
			// models rather than sending it).
			level := req.Thinking.Level
			if level == "" {
				level = ir.LevelFromBudget(req.Thinking.BudgetTokens)
			}
			if level != "" {
				thinking["thinkingLevel"] = strings.ToUpper(level)
			}
		}
		genConfig["thinkingConfig"] = thinking
	} else if req.RequestedEffort != "" {
		genConfig["thinkingConfig"] = map[string]any{
			"thinkingLevel":   strings.ToUpper(req.RequestedEffort),
			"includeThoughts": true,
		}
	}

	inner := map[string]any{"contents": contents}
	if len(sysParts) > 0 {
		inner["systemInstruction"] = map[string]any{"parts": sysParts}
	}
	if len(tools) > 0 {
		inner["tools"] = tools
	}
	if toolConfig != nil {
		inner["toolConfig"] = toolConfig
	}
	if len(genConfig) > 0 {
		inner["generationConfig"] = genConfig
	}
	innerRaw, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	outer := v1InternalRequest{
		Project:     project,
		Model:       upstreamModel,
		RequestType: "agent",
		UserAgent:   "antigravity",
		RequestID:   randHex(16),
		Request:     innerRaw,
	}
	return json.Marshal(outer)
}

func toGeminiParts(parts []ir.ContentPart) []geminiPart {
	var out []geminiPart
	for _, p := range parts {
		switch p.Kind {
		case "text":
			if p.Text != "" {
				out = append(out, geminiPart{Text: p.Text})
			}
		case "image":
			if p.ImageB64 != "" {
				out = append(out, geminiPart{InlineData: &geminiBlob{MIMEType: p.ImageMIME, Data: p.ImageB64}})
			}
		}
	}
	return out
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

func mapFinish(reason string) ir.StopReason {
	switch strings.ToUpper(reason) {
	case "STOP":
		return ir.StopEnd
	case "MAX_TOKENS":
		return ir.StopLength
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST":
		return ir.StopContentFilter
	default:
		return ir.StopEnd
	}
}

// classifyHTTP maps a non-200 response into the normalized error taxonomy.
// detectFrameError surfaces an "error" object inside a 200 SSE frame
// (ticket 36). Status derivation: numeric code when HTTP-shaped, else the
// google.rpc status string (RESOURCE_EXHAUSTED → 429 so the quota-reset
// parser applies); unknown shapes fall back to 500 (transport-style).
func detectFrameError(payload, upstreamModel string) *uperr.Error {
	if !strings.Contains(payload, `"error"`) {
		return nil
	}
	var doc struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return nil
	}
	if doc.Error.Message == "" && doc.Error.Status == "" && doc.Error.Code == nil {
		return nil
	}
	status := 500
	if n, ok := doc.Error.Code.(float64); ok && n >= 400 && n <= 599 {
		status = int(n)
	} else {
		switch strings.ToUpper(doc.Error.Status) {
		case "RESOURCE_EXHAUSTED":
			status = 429
		case "UNAUTHENTICATED":
			status = 401
		case "PERMISSION_DENIED":
			status = 403
		case "INVALID_ARGUMENT", "FAILED_PRECONDITION":
			status = 400
		}
	}
	return classifyHTTP(status, []byte(payload), upstreamModel)
}

func classifyHTTP(status int, body []byte, upstreamModel string) *uperr.Error {
	text := string(body)
	lower := strings.ToLower(text)
	switch {
	case status == 401 || status == 403 && strings.Contains(lower, "credential"):
		return uperr.New(uperr.ClassAuth, status, "antigravity: %s", truncate(body, 300))
	case status == 429 || strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "quota"):
		reset := parseQuotaReset(text)
		e := uperr.New(uperr.ClassSoftRate, status, "antigravity: %s", truncate(body, 300))
		e.ResetAt = reset
		// The delay/reset the body names IS the quota bucket's own boundary
		// (5h / weekly): a hard number, not our guess (BP-135).
		e.ResetHard = !reset.IsZero()
		// Antigravity serves native Gemini and 3P (Claude/GPT) from separate
		// quota ledgers (AIClient2API antigravity-quota-family.js). Google's
		// own 429 ("RESOURCE_EXHAUSTED", "Quota exceeded for quota metric …")
		// names no model, and the old body-contains-"model" heuristic left
		// e.Model empty — so one family's 429 cooled the WHOLE account and
		// took the other family's healthy quota down with it. Every 429 now
		// holds the model that was actually asked for.
		if upstreamModel != "" {
			e.Model = upstreamModel
		}
		return e
	case status == 403:
		if uperr.IsEdgeBlock(body) {
			return uperr.EdgeBlocked(status, "antigravity: edge blocked: %s", truncate(body, 300))
		}
		return uperr.New(uperr.ClassWAF, status, "antigravity: %s", truncate(body, 300))
	case status == 400:
		return uperr.New(uperr.ClassBadRequest, status, "antigravity: %s", truncate(body, 300))
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "antigravity: %s", truncate(body, 300))
	default:
		return uperr.New(uperr.ClassInternal, status, "antigravity: %s", truncate(body, 300))
	}
}

// parseQuotaReset extracts a precise reset moment from a QuotaFailure body.
// Recognized shapes: google.rpc.QuotaFailure details with metadata, a
// "quotaResetDelay":"537s" or "retryDelay":"537s" field, or an RFC3339
// resetTime. Returns the zero time when nothing parseable is found.
func parseQuotaReset(text string) time.Time {
	// retryDelay / quotaResetDelay in seconds: "537s" or plain number.
	for _, key := range []string{`"quotaResetDelay"`, `"retryDelay"`} {
		if v := findStringField(text, key); v != "" {
			if d := parseDelay(v); d > 0 {
				return time.Now().Add(d)
			}
		}
	}
	for _, key := range []string{`"resetTime"`, `"quotaResetTime"`} {
		if v := findStringField(text, key); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

func findStringField(text, key string) string {
	idx := strings.Index(text, key)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(key):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func parseDelay(v string) time.Duration {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "s") {
		v = strings.TrimSuffix(v, "s")
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return 0
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}
