package server

// POST /v1/images/generations — OpenAI-shaped text-to-image (BP-186). The
// accepted contract is deliberately narrow, matching the retired local A2
// proxy this replaces (behaviour spec: research/2026-10-29-antigravity生图通道迁入.md):
//
//	model           whitelist (one entry today: gemini-3.1-flash-image)
//	prompt          required
//	n               1..4, default 1 — one upstream call per image
//	response_format b64_json (default) | url (data: URI, no image host needed)
//	size            accepted and ignored — the upstream forces imageSize=4K

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/protocol/openai"
)

// supportedImageModels mirrors the reference whitelist's gemini entry; the
// gpt/grok image models belong to channels this gateway does not carry.
var supportedImageModels = map[string]bool{
	model.ImageGenModel: true,
}

// imageMaxN caps images per request (reference IMAGE_GEN_MAX_N).
const imageMaxN = 4

type imageGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              any    `json:"n"`
	ResponseFormat string `json:"response_format"`
	Size           any    `json:"size"`
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := "img-" + randToken(12)

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("failed to read body", "invalid_request_error", nil))
		return
	}
	var in imageGenRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(err.Error(), "invalid_request_error", nil))
		return
	}

	// Model: default to the gateway's image model, reject everything outside
	// the whitelist with the list spelled out (the reference's error shape).
	m := strings.TrimSpace(in.Model)
	if m == "" {
		m = model.ImageGenModel
	}
	if !supportedImageModels[strings.ToLower(m)] {
		names := make([]string, 0, len(supportedImageModels))
		for name := range supportedImageModels {
			names = append(names, name)
		}
		writeJSON(w, http.StatusBadRequest, openai.RenderError(
			fmt.Sprintf("model '%s' is not supported; supported image models: %s", m, strings.Join(names, ", ")),
			"invalid_request_error", nil))
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("prompt is required", "invalid_request_error", nil))
		return
	}
	n := coerceImageN(in.N)
	responseFormat := strings.TrimSpace(in.ResponseFormat)
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	if responseFormat != "b64_json" && responseFormat != "url" {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(
			"response_format must be 'b64_json' or 'url'", "invalid_request_error", nil))
		return
	}

	if !s.gw.ImageGenAvailable() {
		writeJSON(w, http.StatusNotImplemented, openai.RenderError(
			"image generation is not available on this deployment", "invalid_request_error", nil))
		return
	}

	logRow := &model.RequestLog{
		Time:      start,
		RequestID: reqID,
		Protocol:  "images",
		Model:     m,
		Stream:    false,
		ClientIP:  clientIP(r),
		UserAgent: r.UserAgent(),
	}
	outcome, err := s.gw.GenerateImages(r.Context(), m, in.Prompt, n)
	logRow.DurationMs = int(time.Since(start).Milliseconds())
	logRow.Attempts = 1
	if outcome != nil {
		logRow.Attempts = max(outcome.Attempts, 1)
		if outcome.Account != nil {
			logRow.AccountID = outcome.Account.ID
			logRow.AccountLabel = accountLabel(outcome.Account)
		}
	}
	logRow.Channel = model.ChannelAntigravity
	if err != nil {
		status, class, _ := gateway.DescribeError(err)
		logRow.Status = status
		logRow.ErrorClass = class
		s.gw.LogRequest(logRow)
		withRetryAfter(w, status, err)
		writeJSON(w, status, openAIError(err))
		return
	}
	logRow.Status = http.StatusOK
	s.gw.LogRequest(logRow)

	data := make([]map[string]any, 0, len(outcome.Images))
	for _, im := range outcome.Images {
		if responseFormat == "url" {
			data = append(data, map[string]any{"url": "data:" + im.MIME + ";base64," + im.B64})
		} else {
			data = append(data, map[string]any{"b64_json": im.B64})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	})
}

// coerceImageN normalizes the n field: at least 1, at most imageMaxN,
// anything unparseable becomes 1 (reference parseInt(body.n) || 1 semantics).
func coerceImageN(v any) int {
	n := 1
	switch t := v.(type) {
	case float64:
		n = int(t)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			n = parsed
		}
	case json.Number:
		if parsed, err := strconv.Atoi(t.String()); err == nil {
			n = parsed
		}
	}
	if n < 1 {
		n = 1
	}
	if n > imageMaxN {
		n = imageMaxN
	}
	return n
}
