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
	"regexp"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/media"
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

// handleImages serves POST /v1/images/generations across every catalog
// channel (BP-306). Enum-capability violations answer capability_not_supported
// with models_that_can (switch-model advice); numeric overflow clamps to the
// model max and reports it in "applied".
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := "img-" + randToken(12)
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("failed to read body", "invalid_request_error", nil))
		return
	}
	var in struct {
		Model          string   `json:"model"`
		Prompt         string   `json:"prompt"`
		N              any      `json:"n"`
		Size           string   `json:"size"`
		AspectRatio    string   `json:"aspect_ratio"`
		Channel        string   `json:"channel"`
		ReferenceImgs  []string `json:"reference_images"`
		Watermark      *bool    `json:"watermark"`
		ResponseFormat string   `json:"response_format"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(err.Error(), "invalid_request_error", nil))
		return
	}
	name := strings.TrimSpace(in.Model)
	if name == "" || strings.EqualFold(name, model.ImageGenModel) {
		name = "nano-banana-2" // legacy alias: the BP-186 antigravity model
	}
	m, ok := media.ByID(name)
	if !ok || m.Kind != "image" {
		writeJSON(w, http.StatusNotFound, openai.RenderError(
			"unknown image model '"+name+"'; see GET /v1/media/models", "invalid_request_error", nil))
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("prompt is required", "invalid_request_error", nil))
		return
	}
	get := func(p string) *media.Param {
		for i := range m.Params {
			if m.Params[i].Name == p {
				return &m.Params[i]
			}
		}
		return nil
	}
	nMax := 4
	if p := get("n"); p != nil && p.Max != nil {
		nMax = *p.Max
	}
	rawN := 1
	if f, perr := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(in.N)), 64); in.N != nil && perr == nil {
		rawN = int(f)
	}
	n := rawN
	applied := map[string]any{}
	if n > nMax {
		applied["n"] = n
		n = nMax
	}
	if n < 1 {
		n = 1
	}
	// Enum capabilities are never rewritten: point at models that can (BP-306 §4.1).
	capReject := func(param, val string) bool {
		if strings.TrimSpace(val) == "" {
			return false
		}
		p := get(param)
		if p != nil && p.Supported {
			// Custom-size params (Pattern set): validate format, total pixels
			// and W/H ratio here — the vendor spec's UI enum is narrower than
			// what it actually accepts (qwen 1:8~8:1 custom, minimax 1:3~3:1).
			if p.Pattern != "" {
				re := regexp.MustCompile(p.Pattern)
				mm := re.FindStringSubmatch(val)
				if mm == nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
						"code":    "invalid_parameter_value",
						"message": param + "='" + val + "' does not match " + p.Pattern + " (W/H separator per model; see GET /v1/media/models note)",
						"param":   param,
						"rule":    sizeRule(p),
					}})
					return true
				}
				// The catalog Pattern carries no capture groups (it is also
				// documented as-is to callers), so split the W/H pair here
				// rather than indexing regex groups that do not exist.
				parts := strings.FieldsFunc(val, func(r rune) bool { return r == '*' || r == 'x' || r == 'X' })
				if len(parts) != 2 {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
						"code":    "invalid_parameter_value",
						"message": param + "='" + val + "' must be W<sep>H (sep = '*' or 'x')",
						"param":   param,
						"rule":    sizeRule(p),
					}})
					return true
				}
				wv, _ := strconv.Atoi(parts[0])
				hv, _ := strconv.Atoi(parts[1])
				px := wv * hv
				if (p.Min != nil && px < *p.Min) || (p.Max != nil && px > *p.Max) {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
						"code":    "invalid_parameter_value",
						"message": fmt.Sprintf("%s='%s' totals %d pixels, allowed %d..%d", param, val, px, deref(p.Min), deref(p.Max)),
						"param":   param,
						"rule":    sizeRule(p),
					}})
					return true
				}
				if hv > 0 {
					r := float64(wv) / float64(hv)
					if (p.RatioMin != nil && r < *p.RatioMin) || (p.RatioMax != nil && r > *p.RatioMax) {
						writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
							"code":    "invalid_parameter_value",
							"message": fmt.Sprintf("%s='%s' ratio %.3f outside %.4g..%.4g", param, val, r, derefF(p.RatioMin), derefF(p.RatioMax)),
							"param":   param,
							"rule":    sizeRule(p),
						}})
						return true
					}
				}
				return false
			}
			for _, v := range p.Values {
				if strings.EqualFold(v, val) {
					return false
				}
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
				"code":    "invalid_parameter_value",
				"message": param + "='" + val + "' is not in model " + m.ID + "'s values: " + strings.Join(p.Values, ", "),
				"param":   param, "allowed": p.Values,
			}})
			return true
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"code":            "capability_not_supported",
			"message":         m.ID + " does not support " + param,
			"param":           param,
			"models_that_can": media.CapabilityModels("image", param),
			"hint":            "switch to one of those models; retrying this one cannot fix it",
		}})
		return true
	}
	if capReject("size", strings.TrimSpace(in.Size)) {
		return
	}
	if capReject("aspect_ratio", strings.TrimSpace(in.AspectRatio)) {
		return
	}
	if len(in.ReferenceImgs) > 0 {
		p := get("reference_images")
		if p == nil || !p.Supported {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
				"code":            "capability_not_supported",
				"message":         m.ID + " does not support reference_images",
				"param":           "reference_images",
				"models_that_can": media.CapabilityModels("image", "reference_images"),
				"hint":            "switch to a model that accepts reference images",
			}})
			return
		}
		if p.Max != nil && len(in.ReferenceImgs) > *p.Max {
			writeJSON(w, http.StatusBadRequest, openai.RenderError(
				fmt.Sprintf("reference_images allows at most %d items for %s", *p.Max, m.ID),
				"invalid_request_error", nil))
			return
		}
	}
	responseFormat := strings.TrimSpace(in.ResponseFormat)
	if responseFormat == "" {
		responseFormat = "url"
	}
	if responseFormat != "b64_json" && responseFormat != "url" {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(
			"response_format must be 'b64_json' or 'url'", "invalid_request_error", nil))
		return
	}

	logRow := &model.RequestLog{
		Time: start, RequestID: reqID, Protocol: "images", Model: m.ID,
		Stream: false, ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	}
	outcome, err := s.gw.GenerateImagesModel(r.Context(), m.ID, strings.TrimSpace(in.Channel), in.Prompt, n, strings.TrimSpace(in.Size), strings.TrimSpace(in.AspectRatio), in.Watermark, in.ReferenceImgs)
	logRow.DurationMs = int(time.Since(start).Milliseconds())
	logRow.Attempts = 1
	if outcome != nil {
		logRow.Attempts = max(outcome.Attempts, 1)
		if outcome.Account != nil {
			logRow.AccountID = outcome.Account.ID
			logRow.AccountLabel = accountLabel(outcome.Account)
			logRow.Channel = outcome.Account.Channel
		}
	}
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
		if responseFormat == "b64_json" && im.B64 != "" {
			data = append(data, map[string]any{"b64_json": im.B64})
		} else if im.URL != "" {
			data = append(data, map[string]any{"url": im.URL})
		} else {
			data = append(data, map[string]any{"b64_json": im.B64})
		}
	}
	resp := map[string]any{"created": time.Now().Unix(), "model": m.ID, "data": data}
	if outcome.Account != nil {
		resp["channel"] = outcome.Account.Channel
		resp["account"] = accountLabel(outcome.Account)
	}
	if len(applied) > 0 {
		resp["applied"] = applied
	}
	resp["note"] = "vendor image URLs expire (workbuddy COS ~24h); persist bytes if needed"
	writeJSON(w, http.StatusOK, resp)
}

// handleMediaModels serves GET /v1/media/models — the caller-facing capability
// catalog (BP-306 P1). gemini-web entries carry requires_browser and stay
// available=false until that channel is wired.
func (s *Server) handleMediaModels(w http.ResponseWriter, r *http.Request) {
	models := media.Catalog()
	out := make([]map[string]any, 0, len(models))
	for i := range models {
		m := &models[i]
		avail := false
		channels := make([]string, 0, len(m.Upstream))
		for ch := range m.Upstream {
			channels = append(channels, ch)
			if ch == "gemini-web" {
				continue
			}
			if s.gw.ChannelAlive(ch) {
				avail = true
			}
		}
		entry := map[string]any{
			"id": m.ID, "kind": m.Kind, "display_name": m.DisplayName,
			"channels": channels, "operations": m.Operations, "params": m.Params,
			"available": avail,
		}
		if m.Realm != nil {
			entry["realm"] = m.Realm
		}
		if m.Elo > 0 {
			entry["quality"] = map[string]any{"elo": m.Elo, "rank": m.EloRank, "source": m.EloSource}
		}
		entry["pricing"] = m.Pricing
		if m.Browser {
			entry["requires_browser"] = true
		}
		if m.Note != "" {
			entry["note"] = m.Note
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
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

// sizeRule serializes the custom-size validation rule into the error body so
// the caller (an LLM) can self-correct without guessing the bounds.
func sizeRule(p *media.Param) map[string]any {
	r := map[string]any{"pattern": p.Pattern}
	if p.Min != nil {
		r["min_pixels"] = *p.Min
	}
	if p.Max != nil {
		r["max_pixels"] = *p.Max
	}
	if p.RatioMin != nil {
		r["ratio_min"] = *p.RatioMin
	}
	if p.RatioMax != nil {
		r["ratio_max"] = *p.RatioMax
	}
	return r
}

func deref(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefF(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
