// Package server exposes the HTTP API: OpenAI chat completions, models, and
// health. Protocol-specific rendering lives in the protocol packages; this
// package wires HTTP semantics (auth, SSE framing, error status mapping).
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/protocol/openai"
)

// Server is the HTTP front end.
type Server struct {
	cfg   *config.Config
	gw    *gateway.Gateway
	panel PanelMounter
}

// PanelMounter mounts panel routes onto the mux (implemented by panel.Panel).
type PanelMounter interface {
	Enabled() bool
	Mount(mux *http.ServeMux)
}

// New builds the server.
func New(cfg *config.Config, gw *gateway.Gateway) *Server {
	return &Server{cfg: cfg, gw: gw}
}

// WithPanel attaches the management panel.
func (s *Server) WithPanel(p PanelMounter) *Server {
	s.panel = p
	return s
}

// Handler returns the root http.Handler with all routes mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChatCompletions))
	mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	s.MountProtocols(mux)
	if s.panel != nil && s.panel.Enabled() {
		s.panel.Mount(mux)
	}
	return mux
}

// auth enforces the optional Bearer key (constant-time compare).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				// Anthropic SDKs authenticate every call — model lists
				// included — with x-api-key, not Bearer (misakano dc75c04):
				// without this fallback such a client calls /v1/messages fine
				// and then 401s on GET /v1/models.
				got = r.Header.Get("x-api-key")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIKey)) != 1 {
				writeJSON(w, http.StatusUnauthorized, openai.RenderError("invalid api key", "invalid_request_error", "invalid_api_key"))
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "bargainproxy"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	infos := s.gw.ListModels()
	data := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		var owned []string
		natives := make([]string, 0, len(mi.Channels))
		for _, c := range mi.Channels {
			owned = append(owned, c.Channel)
			natives = append(natives, c.Native)
		}
		entry := map[string]any{
			"id":       mi.Model,
			"object":   "model",
			"created":  1753600000,
			"owned_by": strings.Join(owned, "+"),
		}
		// context_length: static knowledge table with real values; unknown
		// models omit the field entirely (no padded 1M default).
		if n, ok := contextLengthFor(mi.Model, natives); ok {
			entry["context_length"] = n
		}
		// pricing: freshest observed cost tier (0 free / 2 paid); omitted
		// when no observation exists — never guessed.
		if tier, ok := costTierFor(s.gw, mi.Model, natives); ok {
			entry["pricing"] = map[string]any{"tier": tier, "note": tierNote(tier)}
		}
		data = append(data, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// contextLengthFor resolves the static context window: the canonical name
// first, then the smallest known native binding value.
func contextLengthFor(model string, natives []string) (int, bool) {
	if n, ok := config.ContextLength(model); ok {
		return n, true
	}
	best, found := 0, false
	for _, n := range natives {
		if v, ok := config.ContextLength(n); ok && (!found || v < best) {
			best, found = v, true
		}
	}
	return best, found
}

// costTierFor aggregates the freshest observation across the model and its
// native bindings (most favorable tier wins: free-first is the product goal).
func costTierFor(gw *gateway.Gateway, model string, natives []string) (int, bool) {
	best, found := 0, false
	for _, name := range append([]string{model}, natives...) {
		if t, ok := gw.CostTier(name); ok && (!found || t < best) {
			best, found = t, true
		}
	}
	return best, found
}

func tierNote(tier int) string {
	if tier == 0 {
		return "free (observed)"
	}
	return "paid (observed)"
}

// trackingWriter records whether headers were sent (stream error handling
// needs to know if a JSON error can still be written).
type trackingWriter struct {
	http.ResponseWriter
	sent bool
}

func (t *trackingWriter) WriteHeader(status int) {
	t.sent = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.sent = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Flush() {
	if fl, ok := t.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func headersSent(w http.ResponseWriter) bool {
	if tw, ok := w.(*trackingWriter); ok {
		return tw.sent
	}
	return true // unknown writer: assume committed (safe default)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	tw := &trackingWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	reqID := "chatcmpl-" + randToken(12)
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("failed to read body", "invalid_request_error", nil))
		return
	}
	req, err := openai.ParseChatRequest(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(err.Error(), "invalid_request_error", nil))
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("model is required", "invalid_request_error", nil))
		return
	}
	req.Model = gateway.TrimModelName(req.Model)
	applyNativeSession(r, req)

	// Policy: no server-side fetching of external image URLs.
	if url, bad := gateway.HasUnsupportedImageURL(req); bad {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(
			fmt.Sprintf("external image URL is not supported (use base64 data URLs): %s", url),
			"invalid_request_error", "unsupported_image_url"))
		return
	}

	logRow := &model.RequestLog{
		Time:      start,
		RequestID: reqID,
		Protocol:  "openai-chat",
		Model:     gateway.BareModel(req.Model),
		Stream:    req.Stream,
		ClientIP:  clientIP(r),
		UserAgent: r.UserAgent(),
	}

	var result *gateway.Result
	if req.Stream {
		result, err = s.streamChat(w, r, req, reqID, start, logRow)
	} else {
		result, err = s.bufferedChat(w, r, req, reqID, start, logRow)
	}
	if err != nil {
		status, class, _ := gateway.DescribeError(err)
		logRow.Status = status
		logRow.ErrorClass = class
		logRow.DurationMs = int(time.Since(start).Milliseconds())
		s.gw.LogRequest(logRow)
		if !headersSent(w) {
			withRetryAfter(w, status, err)
			writeJSON(w, status, openAIError(err))
		}
		return
	}
	logRow.Status = 200
	logRow.Channel = result.Channel
	logRow.AccountID = result.Account.ID
	logRow.AccountLabel = accountLabel(result.Account)
	logRow.DurationMs = int(time.Since(start).Milliseconds())
	if result.Response != nil {
		logRow.InputTokens = result.Response.Usage.InputTokens
		logRow.OutputTokens = result.Response.Usage.OutputTokens
		logRow.Credit = result.Response.Usage.Credit
		logRow.CreditKnown = result.Response.Usage.CreditKnown
		logRow.CachedTokens = result.Response.Usage.CachedInputTokens
		logRow.CacheKnown = result.Response.Usage.CacheKnown
		logRow.ToolCalls = len(result.Response.Message.ToolCalls)
	}
	logRow.ToolsDeclared = result.ToolsDeclared
	if result.Degraded != "" {
		logRow.Degraded = result.Degraded
	}
	s.gw.LogRequest(logRow)
}

// execute runs gw.Execute with an attempt Trace attached and folds the
// cascade trail onto the log row afterwards (success or failure) — BP-38.
// On failure the last attempted channel AND account are recorded so error rows
// stop showing empty channel/account columns (account attribution added by
// BP-107: without it "which account dropped this connection?" was answerable
// only by guessing from the upstream IP inside the error text).
func (s *Server) execute(r *http.Request, req *ir.ChatRequest, emit func(ir.StreamEvent) error, logRow *model.RequestLog) (*gateway.Result, error) {
	tr := &gateway.Trace{}
	res, err := s.gw.Execute(gateway.WithTrace(r.Context(), tr), req, emit)
	logRow.Attempts = tr.Attempts
	if len(tr.Channels) > 0 {
		logRow.AttemptChannels = strings.Join(tr.Channels, "→")
		if err != nil && logRow.Channel == "" {
			logRow.Channel = tr.Channels[len(tr.Channels)-1]
		}
	}
	if err != nil && logRow.AccountID == "" && tr.LastAccount != nil {
		logRow.AccountID = tr.LastAccount.ID
		logRow.AccountLabel = accountLabel(tr.LastAccount)
	}
	return res, err
}

// bufferedChat runs a non-streamed request: the gateway aggregates the
// upstream stream and we render one JSON completion.
func (s *Server) bufferedChat(w http.ResponseWriter, r *http.Request, req *ir.ChatRequest, reqID string, start time.Time, logRow *model.RequestLog) (*gateway.Result, error) {
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
		return nil, err
	}
	logRow.TTFBMs = ttfb
	created := result.Response.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	out := openai.RenderCompletion(reqID, req.Model, created, result.Response)
	if result.Degraded != "" {
		w.Header().Set("X-BP-Served-Channel", result.Degraded)
	}
	writeJSON(w, http.StatusOK, out)
	return result, nil
}

// streamChat runs a streamed request with OpenAI SSE framing.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *ir.ChatRequest, reqID string, start time.Time, logRow *model.RequestLog) (*gateway.Result, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(http.StatusOK)

	// BP-142: a Chat client only understands data frames, so an SSE comment
	// is the one keepalive we may send while the upstream is quiet — see
	// keepalive.go for why silence is not survivable without it.
	lw, ka := newStreamKeepalive(w, "")
	defer ka.Stop()

	created := time.Now().Unix()
	first := true
	includeUsage := false
	if v, ok := req.Raw["include_usage"].(bool); ok {
		includeUsage = v
	}
	var lastUsage *ir.Usage

	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(lw, "data: %s\n\n", b); err != nil {
			return err
		}
		return nil
	}

	result, err := s.execute(r, req, func(ev ir.StreamEvent) error {
		if first {
			logRow.TTFBMs = int(time.Since(start).Milliseconds())
			first = false
		}
		ka.MarkStarted()
		ka.NoteEvent()
		switch ev.Kind {
		case ir.EvText:
			d := &openai.RespDelta{Role: "assistant", Content: ev.Delta}
			return send(openai.RenderChunk(reqID, req.Model, created, d, nil))
		case ir.EvThinking:
			d := &openai.RespDelta{Role: "assistant", ReasoningContent: ev.Delta}
			return send(openai.RenderChunk(reqID, req.Model, created, d, nil))
		case ir.EvToolStart:
			var tc openai.RespToolCall
			idx := ev.ToolIndex
			tc.Index = &idx
			tc.ID = ev.ToolCall.ID
			tc.Type = "function"
			tc.Function.Name = ev.ToolCall.Name
			d := &openai.RespDelta{Role: "assistant", ToolCalls: []openai.RespToolCall{tc}}
			return send(openai.RenderChunk(reqID, req.Model, created, d, nil))
		case ir.EvToolArgs:
			var tc openai.RespToolCall
			idx := ev.ToolIndex
			tc.Index = &idx
			tc.Function.Arguments = ev.Delta
			d := &openai.RespDelta{ToolCalls: []openai.RespToolCall{tc}}
			return send(openai.RenderChunk(reqID, req.Model, created, d, nil))
		case ir.EvUsage:
			u := *ev.Usage
			lastUsage = &u
			if includeUsage {
				return send(openai.RenderUsageChunk(reqID, req.Model, created, openai.UsageFromIR(*ev.Usage)))
			}
			return nil
		case ir.EvDone:
			finish := string(ev.Stop)
			if err := send(openai.RenderChunk(reqID, req.Model, created, &openai.RespDelta{}, &finish)); err != nil {
				return err
			}
			_, err := lw.Write([]byte("data: [DONE]\n\n"))
			return err
		}
		return nil
	}, logRow)
	if err != nil {
		// Headers already sent: terminate the stream with a trailing error
		// chunk and [DONE] so clients don't hang.
		if headersSent(w) {
			_ = send(openAIError(err))
			_, _ = lw.Write([]byte("data: [DONE]\n\n"))
		}
		return nil, err
	}
	if lastUsage != nil && includeUsage {
		// already sent inline; nothing further
	}
	return result, nil
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func accountLabel(a *model.Account) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	if a.UID != "" {
		return a.UID
	}
	return a.ID
}

// randToken returns n random hex characters.
func randToken(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}
