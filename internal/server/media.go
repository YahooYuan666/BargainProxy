package server

// Video generation endpoints (BP-306):
//   POST /v1/videos/generations      → 202 + pollable task
//   GET  /v1/videos/generations/{id} → task status / result URLs
// Video vendor URLs expire like image URLs; the note says so on every result.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/media"
	"bargainproxy/internal/model"
	"bargainproxy/internal/protocol/openai"
)

func (s *Server) handleVideoGenerations(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError("failed to read body", "invalid_request_error", nil))
		return
	}
	var in struct {
		Model       string `json:"model"`
		Prompt      string `json:"prompt"`
		Resolution  string `json:"resolution"`
		AspectRatio string `json:"aspect_ratio"`
		Channel     string `json:"channel"`
		FirstFrame  string `json:"first_frame"`
		LastFrame   string `json:"last_frame"`
		Duration    any    `json:"duration"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(err.Error(), "invalid_request_error", nil))
		return
	}
	name := strings.TrimSpace(in.Model)
	if name == "" {
		name = "minimax-video-h3" // catalog default (Elo 3, both channels)
	}
	m, ok := media.ByID(name)
	if !ok || m.Kind != "video" {
		writeJSON(w, http.StatusNotFound, openai.RenderError(
			"unknown video model '"+name+"'; see GET /v1/media/models", "invalid_request_error", nil))
		return
	}
	if strings.TrimSpace(in.Prompt) == "" && strings.TrimSpace(in.FirstFrame) == "" {
		writeJSON(w, http.StatusBadRequest, openai.RenderError(
			"prompt is required for text-to-video; first_frame alone is not enough", "invalid_request_error", nil))
		return
	}
	duration := 5
	if v, err := coerceInt(in.Duration); err == nil {
		duration = v
	}
	logRow := &model.RequestLog{
		Time: start, RequestID: "vid-" + randToken(12), Protocol: "videos", Model: m.ID,
		Stream: false, ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	}
	task, err := s.gw.GenerateVideo(r.Context(), m.ID, strings.TrimSpace(in.Channel), in.Prompt,
		strings.TrimSpace(in.Resolution), strings.TrimSpace(in.AspectRatio), strings.TrimSpace(in.FirstFrame),
		strings.TrimSpace(in.LastFrame), duration)
	logRow.DurationMs = int(time.Since(start).Milliseconds())
	logRow.Attempts = 1
	logRow.Channel = model.ChannelWorkBuddy
	if task != nil {
		logRow.Channel = ""
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
	logRow.Status = http.StatusAccepted
	s.gw.LogRequest(logRow)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": task.ID, "status": task.Status, "model": m.ID,
		"poll": "/v1/videos/generations/" + task.ID,
	})
}

func (s *Server) handleVideoTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, ok := s.gw.VideoTask(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, openai.RenderError(
			"unknown task '"+id+"'", "not_found_error", nil))
		return
	}
	resp := map[string]any{
		"id": task.ID, "status": task.Status, "model": task.ModelID,
	}
	if task.Channel != "" {
		resp["channel"] = task.Channel
	}
	if task.Account != "" {
		resp["account"] = task.Account
	}
	if task.Attempts > 0 {
		resp["attempts"] = task.Attempts
	}
	if task.Error != "" {
		resp["error"] = task.Error
	}
	if len(task.URLs) > 0 {
		urls := make([]string, 0, len(task.URLs))
		urls = append(urls, task.URLs...)
		resp["urls"] = urls
		resp["mime"] = task.MIME
		resp["note"] = "vendor URLs expire; download and persist the bytes"
	}
	writeJSON(w, http.StatusOK, resp)
}

// coerceInt normalizes the duration field (number or numeric string).
func coerceInt(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, errNotNumeric
}

// errNotNumeric marks a duration value that is neither a number nor digits.
var errNotNumeric = errors.New("duration is not a number")
