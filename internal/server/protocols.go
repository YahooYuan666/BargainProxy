// Protocol handlers: Anthropic Messages, OpenAI Responses, Gemini v1beta.
// They share the gateway execution path with the OpenAI chat handler; only
// parsing and rendering differ.
package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/protocol/anthropic"
	"bargainproxy/internal/protocol/geminiv1"
	"bargainproxy/internal/protocol/responses"
	"bargainproxy/internal/sessionfp"
	"bargainproxy/internal/uperr"
)

// MountProtocols registers the extra protocol routes on the mux.
func (s *Server) MountProtocols(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", s.auth(s.handleAnthropicMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.auth(s.handleCountTokens))
	mux.HandleFunc("POST /v1/responses", s.auth(s.handleResponses))
	mux.HandleFunc("POST /v1beta/models/{model}", s.auth(s.handleGeminiGenerate))
}

// applyNativeSession prioritises the session id the client names in its own
// headers over any fingerprint derived from message content. A client that
// names its conversation knows the truth; content digests can collide when
// two chats open with the same boilerplate (2026-10-20 cross-talk bug).
func applyNativeSession(r *http.Request, req *ir.ChatRequest) {
	if req == nil || r == nil {
		return
	}
	if v := sessionfp.FromHeader(r.Header.Get); v != "" {
		req.SessionKey = v
	}
}

// --- Anthropic Messages ---

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	tw := &trackingWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	reqID := "msg_" + randToken(12)

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, anthropic.RenderError("invalid_request_error", "failed to read body"))
		return
	}
	req, err := anthropic.ParseMessagesRequest(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, anthropic.RenderError("invalid_request_error", err.Error()))
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, anthropic.RenderError("invalid_request_error", "model is required"))
		return
	}
	req.Model = gateway.TrimModelName(req.Model)
	// This protocol's stream writer buffers tool blocks until the turn ends
	// (BP-70) — the gateway may retry a mid-tool transport drop on another
	// attempt, because the client has then received nothing (BP-123).
	req.ToolDeltasBuffered = true
	applyNativeSession(r, req)
	if url, bad := gateway.HasUnsupportedImageURL(req); bad {
		writeJSON(w, http.StatusBadRequest, anthropic.RenderError("invalid_request_error",
			"external image URL is not supported (use base64 data URLs): "+url))
		return
	}

	logRow := &model.RequestLog{
		Time: start, RequestID: reqID, Protocol: "anthropic-messages", Model: gateway.BareModel(req.Model),
		Stream: req.Stream, ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	}

	if req.Stream {
		var sw *anthropic.StreamWriter
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		// BP-142: keep the client alive while the upstream is silent. Before
		// message_start only SSE comments are legal; after it, one ping per
		// minute at most (see keepalive.go).
		lw, ka := newStreamKeepalive(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		defer ka.Stop()
		// The estimate fills message_start's prompt accounting until the
		// upstream reports the real numbers in the final usage (BP-56).
		sw = anthropic.NewStreamWriter(lw, lw, reqID, req.Model, anthropic.EstimateTokens(req))
		// Capture the first-byte time on the streaming path too (BP-40):
		// 反应时间 is the metric the panel's log/efficiency columns need, and
		// streaming is how the Claude-protocol clients actually talk.
		first := true
		emit := func(ev ir.StreamEvent) error {
			if first {
				logRow.TTFBMs = int(time.Since(start).Milliseconds())
				first = false
			}
			if err := sw.Send(ev); err != nil {
				return err
			}
			// Send writes message_start on its first call, so the stream is
			// past the "comments only" window from here on.
			ka.MarkStarted()
			ka.NoteEvent()
			return nil
		}
		result, err := s.execute(r, req, emit, logRow)
		if err != nil {
			_, _, msg := gateway.DescribeError(err)
			if tw.sent {
				_ = sw.FinishWithError(msg)
			}
			logRow.Status, logRow.ErrorClass, logRow.DurationMs = statusOf(err), classOf(err), int(time.Since(start).Milliseconds())
			s.gw.LogRequest(logRow)
			return
		}
		logResult(logRow, result, start)
		s.gw.LogRequest(logRow)
		return
	}

	ttfb := 0
	first := true
	result, err := s.execute(r, req, func(ev ir.StreamEvent) error {
		if first {
			ttfb = int(time.Since(start).Milliseconds())
			first = false
		}
		return nil
	}, logRow)
	if err != nil {
		status, class, msg := gateway.DescribeError(err)
		logRow.Status, logRow.ErrorClass, logRow.DurationMs = status, class, int(time.Since(start).Milliseconds())
		s.gw.LogRequest(logRow)
		withRetryAfter(w, status, err)
		writeJSON(w, status, anthropic.RenderError(kindForClass(class), msg))
		return
	}
	logRow.TTFBMs = ttfb
	created := result.Response.CreatedAt
	_ = created
	out := anthropic.RenderMessage(reqID, req.Model, result.Response)
	if result.Degraded != "" {
		w.Header().Set("X-BP-Served-Channel", result.Degraded)
	}
	writeJSON(w, http.StatusOK, out)
	logResult(logRow, result, start)
	s.gw.LogRequest(logRow)
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, anthropic.RenderError("invalid_request_error", "failed to read body"))
		return
	}
	req, err := anthropic.ParseMessagesRequest(body)
	if err != nil {
		// Contract (AIClient2API parity): never block the client; return 0.
		writeJSON(w, http.StatusOK, anthropic.CountTokensResponse{InputTokens: 0})
		return
	}
	writeJSON(w, http.StatusOK, anthropic.CountTokensResponse{InputTokens: anthropic.EstimateTokens(req)})
}

// --- OpenAI Responses ---

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	tw := &trackingWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	reqID := "resp_" + randToken(12)

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, responses.RenderError("invalid_request_error", "failed to read body"))
		return
	}
	req, err := responses.ParseRequest(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, responses.RenderError("invalid_request_error", err.Error()))
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, responses.RenderError("invalid_request_error", "model is required"))
		return
	}
	req.Model = gateway.TrimModelName(req.Model)
	applyNativeSession(r, req)
	if url, bad := gateway.HasUnsupportedImageURL(req); bad {
		writeJSON(w, http.StatusBadRequest, responses.RenderError("invalid_request_error",
			"external image URL is not supported (use base64 data URLs): "+url))
		return
	}

	logRow := &model.RequestLog{
		Time: start, RequestID: reqID, Protocol: "openai-responses", Model: gateway.BareModel(req.Model),
		Stream: req.Stream, ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		// BP-142: Responses carries a sequence_number on every event, so we
		// must not invent frames — a comment is the only legal keepalive.
		lw, ka := newStreamKeepalive(w, "")
		defer ka.Stop()
		sw := responses.NewStreamWriter(lw, lw, reqID, req.Model)
		first := true
		emit := func(ev ir.StreamEvent) error {
			if first {
				logRow.TTFBMs = int(time.Since(start).Milliseconds())
				first = false
			}
			if err := sw.Send(ev); err != nil {
				return err
			}
			ka.MarkStarted()
			ka.NoteEvent()
			return nil
		}
		result, err := s.execute(r, req, emit, logRow)
		if err != nil {
			_, _, msg := gateway.DescribeError(err)
			if tw.sent {
				// BP-244: name an overflow in the vocabulary the client acts on.
				if ue, ok := uperr.As(err); ok && ue.ContextOverflow {
					_ = sw.FinishWithErrorCode("context_length_exceeded", msg)
				} else {
					_ = sw.FinishWithError(msg)
				}
			}
			logRow.Status, logRow.ErrorClass, logRow.DurationMs = statusOf(err), classOf(err), int(time.Since(start).Milliseconds())
			s.gw.LogRequest(logRow)
			return
		}
		logResult(logRow, result, start)
		s.gw.LogRequest(logRow)
		return
	}

	var textBuf strings.Builder
	ttfb := 0
	first := true
	result, err := s.execute(r, req, func(ev ir.StreamEvent) error {
		if first {
			ttfb = int(time.Since(start).Milliseconds())
			first = false
		}
		if ev.Kind == ir.EvText {
			textBuf.WriteString(ev.Delta)
		}
		return nil
	}, logRow)
	if err != nil {
		status, class, msg := gateway.DescribeError(err)
		logRow.Status, logRow.ErrorClass, logRow.DurationMs = status, class, int(time.Since(start).Milliseconds())
		s.gw.LogRequest(logRow)
		withRetryAfter(w, status, err)
		// BP-244: a context overflow must be told in the vocabulary this
		// client acts on. Codex compacts only on `context_length_exceeded`;
		// with anything else it resends the same oversized body.
		if ue, ok := uperr.As(err); ok && ue.ContextOverflow {
			writeJSON(w, status, responses.RenderContextOverflow(msg))
			return
		}
		writeJSON(w, status, openAIError(err))
		return
	}
	logRow.TTFBMs = ttfb
	created := result.Response.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	out := responses.Render(reqID, req.Model, created, result.Response, textBuf.String())
	if result.Degraded != "" {
		w.Header().Set("X-BP-Served-Channel", result.Degraded)
	}
	writeJSON(w, http.StatusOK, out)
	logResult(logRow, result, start)
	s.gw.LogRequest(logRow)
}

// --- Gemini v1beta ---

func (s *Server) handleGeminiGenerate(w http.ResponseWriter, r *http.Request) {
	tw := &trackingWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	reqID := "gemini_" + randToken(10)

	// Path: /v1beta/models/{model}:generateContent | :streamGenerateContent
	pathModel := r.PathValue("model")
	modelName, method, cut := strings.Cut(pathModel, ":")
	if !cut {
		writeJSON(w, http.StatusNotFound, geminiv1.RenderError(404, "NOT_FOUND", "unsupported path"))
		return
	}
	streaming := method == "streamGenerateContent"
	if method != "generateContent" && !streaming {
		writeJSON(w, http.StatusNotFound, geminiv1.RenderError(404, "NOT_FOUND", "unsupported method "+method))
		return
	}
	altSSE := r.URL.Query().Get("alt") == "sse"
	if streaming && !altSSE {
		// Native SDKs use alt=sse; without it, aggregate and return one body.
		streaming = false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, geminiv1.RenderError(400, "INVALID_ARGUMENT", "failed to read body"))
		return
	}
	session := r.Header.Get("X-Session-Id")
	req, err := geminiv1.ParseRequest(body, sessionKeyFromXD(session))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, geminiv1.RenderError(400, "INVALID_ARGUMENT", err.Error()))
		return
	}
	// This protocol's stream writer holds tool calls until finalize — the
	// gateway may retry a mid-tool transport drop on another attempt (BP-123).
	req.ToolDeltasBuffered = true
	req.Model = gateway.TrimModelName(modelName)
	applyNativeSession(r, req)
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, geminiv1.RenderError(400, "INVALID_ARGUMENT", "model is required"))
		return
	}
	if url, bad := gateway.HasUnsupportedImageURL(req); bad {
		writeJSON(w, http.StatusBadRequest, geminiv1.RenderError(400, "INVALID_ARGUMENT",
			"external image URL is not supported (use base64 data URLs): "+url))
		return
	}

	logRow := &model.RequestLog{
		Time: start, RequestID: reqID, Protocol: "gemini-v1beta", Model: gateway.BareModel(req.Model),
		Stream: streaming, ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	}

	req.Stream = streaming // the gateway needs stream-ness for no-duplicate cascading (ticket 28)
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		// BP-142: Gemini's SSE framing is comment-tolerant; it has no
		// keepalive event of its own, so comments only (magpie sends nothing
		// for Gemini for the same reason).
		lw, ka := newStreamKeepalive(w, "")
		defer ka.Stop()
		sw := geminiv1.NewStreamWriter(lw, lw, req.Model, req.Thinking != nil && req.Thinking.Include)
		first := true
		emit := func(ev ir.StreamEvent) error {
			if first {
				logRow.TTFBMs = int(time.Since(start).Milliseconds())
				first = false
			}
			if err := sw.Send(ev); err != nil {
				return err
			}
			ka.MarkStarted()
			ka.NoteEvent()
			return nil
		}
		result, err := s.execute(r, req, emit, logRow)
		if err != nil {
			_, _, msg := gateway.DescribeError(err)
			if tw.sent {
				_ = sw.FinishWithError(msg)
			}
			logRow.Status, logRow.ErrorClass, logRow.DurationMs = statusOf(err), classOf(err), int(time.Since(start).Milliseconds())
			s.gw.LogRequest(logRow)
			return
		}
		logResult(logRow, result, start)
		s.gw.LogRequest(logRow)
		return
	}

	var textBuf strings.Builder
	ttfb := 0
	first := true
	result, err := s.execute(r, req, func(ev ir.StreamEvent) error {
		if first {
			ttfb = int(time.Since(start).Milliseconds())
			first = false
		}
		if ev.Kind == ir.EvText {
			textBuf.WriteString(ev.Delta)
		}
		return nil
	}, logRow)
	if err != nil {
		status, class, msg := gateway.DescribeError(err)
		logRow.Status, logRow.ErrorClass, logRow.DurationMs = status, class, int(time.Since(start).Milliseconds())
		s.gw.LogRequest(logRow)
		withRetryAfter(w, status, err)
		writeJSON(w, status, geminiv1.RenderError(status, googleState(status), msg))
		return
	}
	logRow.TTFBMs = ttfb
	out := geminiv1.Render(req.Model, result.Response, req.Thinking != nil && req.Thinking.Include)
	if result.Degraded != "" {
		w.Header().Set("X-BP-Served-Channel", result.Degraded)
	}
	writeJSON(w, http.StatusOK, out)
	logResult(logRow, result, start)
	s.gw.LogRequest(logRow)
}

func sessionKeyFromXD(xd string) string {
	if xd == "" {
		return ""
	}
	return "xid:" + xd
}

func googleState(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 500:
		return "INTERNAL"
	case 502, 503:
		return "UNAVAILABLE"
	default:
		return "UNKNOWN"
	}
}

// --- shared logging helpers ---

func statusOf(err error) int {
	status, _, _ := gateway.DescribeError(err)
	return status
}

func classOf(err error) string {
	_, class, _ := gateway.DescribeError(err)
	return class
}

func kindForClass(class string) string {
	switch class {
	case "auth":
		return "authentication_error"
	case "bad_request", "content_blocked", "sentinel":
		return "invalid_request_error"
	case "quota_exhausted", "soft_rate", "model_rate":
		return "rate_limit_error"
	case "upstream_busy":
		return "unavailable_error"
	default:
		return "api_error"
	}
}

// retryAfterSeconds derives a Retry-After value (whole seconds) from an error
// carrying an upstream reset moment — or the gateway's own hold end when the
// pool annotated one (the hold is capped, so retrying then probes for an early
// recovery). sub2api parity (BP-47): upstream rate limits answer 429 +
// Retry-After so SDKs back off on their own instead of hammering.
func retryAfterSeconds(err error) int {
	ue, ok := uperr.As(err)
	if !ok {
		return 0
	}
	t := ue.HoldUntil
	if t.IsZero() {
		t = ue.ResetAt
	}
	if t.IsZero() {
		return 0
	}
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	s := int(d.Seconds())
	if s < 1 {
		s = 1
	}
	return s
}

// withRetryAfter sets Retry-After on 429 and 503 responses when the reset
// moment is known. 503 upstream-busy carries one too (qoderhub #99: an
// aggregator without the header retries immediately and deepens the queue).
// The value is capped at 600s: CLI clients sleep delayMs =
// Retry-After*1000 with no ceiling of their own, and a multi-hour model
// cooldown must not hang them for hours (misakano 436631c). Call before
// writeJSON.
func withRetryAfter(w http.ResponseWriter, status int, err error) {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return
	}
	if ra := retryAfterSeconds(err); ra > 0 {
		if ra > maxRetryAfterSeconds {
			ra = maxRetryAfterSeconds
		}
		w.Header().Set("Retry-After", strconv.Itoa(ra))
	}
}

// maxRetryAfterSeconds caps the Retry-After header we hand to clients.
const maxRetryAfterSeconds = 600

func logResult(row *model.RequestLog, result *gateway.Result, start time.Time) {
	row.Status = 200
	row.Channel = result.Channel
	row.AccountID = result.Account.ID
	row.AccountLabel = accountLabel(result.Account)
	row.DurationMs = int(time.Since(start).Milliseconds())
	if result.Response != nil {
		row.InputTokens = result.Response.Usage.InputTokens
		row.OutputTokens = result.Response.Usage.OutputTokens
		row.Credit = result.Response.Usage.Credit
		row.CreditKnown = result.Response.Usage.CreditKnown
		row.CachedTokens = result.Response.Usage.CachedInputTokens
		row.CacheKnown = result.Response.Usage.CacheKnown
		row.ToolCalls = len(result.Response.Message.ToolCalls)
	}
	row.ToolsDeclared = result.ToolsDeclared
	if result.Degraded != "" {
		row.Degraded = result.Degraded
	}
}
