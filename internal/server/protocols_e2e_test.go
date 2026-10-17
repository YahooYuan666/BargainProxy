package server_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAnthropicMessagesEndpoint drives /v1/messages against the mock
// antigravity upstream (non-streaming + streaming + count_tokens).
func TestAnthropicMessagesEndpoint(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)

	// Non-streaming.
	body := `{"model":"gemini-3-pro","max_tokens":50,"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Fatalf("envelope: %+v", out)
	}
	if len(out.Content) == 0 || out.Content[0].Text != "pong" {
		t.Fatalf("content: %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop: %s", out.StopReason)
	}
	if out.Usage.InputTokens != 3 || out.Usage.OutputTokens != 1 {
		t.Fatalf("usage: %+v", out.Usage)
	}

	// Streaming: assert event sequence.
	sbody := `{"model":"gemini-3-pro","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	sresp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(sbody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer sresp.Body.Close()
	if ct := sresp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type: %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(sresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in sequence: %s", want, joined)
		}
	}

	// count_tokens never fails.
	cbody := `{"model":"gemini-3-pro","messages":[{"role":"user","content":"hello world"}]}`
	cresp, err := http.Post(srv.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(cbody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != 200 {
		t.Fatalf("count_tokens status %d", cresp.StatusCode)
	}
	var ct struct {
		InputTokens int `json:"input_tokens"`
	}
	_ = json.NewDecoder(cresp.Body).Decode(&ct)
	if ct.InputTokens < 1 {
		t.Fatalf("count_tokens = %+v", ct)
	}
}

func TestResponsesEndpoint(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)

	// Non-streaming.
	body := `{"model":"gemini-3-pro","input":"ping"}`
	resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Object     string `json:"object"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []any  `json:"output"`
		Usage      struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "response" || out.Status != "completed" || out.OutputText != "pong" {
		t.Fatalf("response: %+v", out)
	}
	if out.Usage.TotalTokens != 4 {
		t.Fatalf("usage: %+v", out.Usage)
	}

	// Streaming: assert the evented state machine markers.
	sbody := `{"model":"gemini-3-pro","stream":true,"input":"ping"}`
	sresp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(sbody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer sresp.Body.Close()
	var events []string
	sc := bufio.NewScanner(sresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_text.delta", "response.output_text.done", "response.completed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in: %s", want, joined)
		}
	}
}

func TestGeminiEndpoint(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)

	// Non-streaming generateContent.
	body := `{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`
	resp, err := http.Post(srv.URL+"/v1beta/models/gemini-3-pro:generateContent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			TotalTokenCount int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].Content.Role != "model" {
		t.Fatalf("candidates: %+v", out.Candidates)
	}
	if len(out.Candidates[0].Content.Parts) == 0 || out.Candidates[0].Content.Parts[0].Text != "pong" {
		t.Fatalf("parts: %+v", out.Candidates[0].Content.Parts)
	}
	if out.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("finish: %s", out.Candidates[0].FinishReason)
	}

	// SSE streaming.
	sresp, err := http.Post(srv.URL+"/v1beta/models/gemini-3-pro:streamGenerateContent?alt=sse", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer sresp.Body.Close()
	if ct := sresp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type: %q", ct)
	}
	sc := bufio.NewScanner(sresp.Body)
	var dataFrames int
	var lastFrame string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			dataFrames++
			lastFrame = line
		}
		if strings.Contains(line, "[DONE]") {
			t.Fatal("stream must not contain [DONE]")
		}
	}
	if dataFrames == 0 {
		t.Fatal("no SSE frames received")
	}
	if !strings.Contains(lastFrame, `"finishReason":"STOP"`) {
		t.Fatalf("last frame missing finishReason: %s", lastFrame)
	}
}
