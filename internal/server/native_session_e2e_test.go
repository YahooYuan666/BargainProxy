package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/model"
	"bargainproxy/internal/sessionfp"
)

// TestNativeSessionHeaderBindsStickyAccount: two requests with IDENTICAL
// bodies but different X-Conversation-Id values must produce two distinct
// sticky bindings. Without header-based identity both hash the same prefix
// into ONE binding — the 2026-10-20 cross-talk failure.
func TestNativeSessionHeaderBindsStickyAccount(t *testing.T) {
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEAs(w, "trae")
	}))
	defer traeUp.Close()
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEAs(w, "wb")
	}))
	defer wbUp.Close()

	srv, p := buildCrossChannelServer(t, traeUp, wbUp)

	post := func(conv string) {
		body := `{"model":"glm-5.3","messages":[{"role":"user","content":"same opening boilerplate"}]}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", conv)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", conv, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	post("conv-alpha")
	post("conv-beta")

	keyA := sessionfp.FromHeader(func(h string) string {
		if strings.EqualFold(h, "X-Conversation-Id") {
			return "conv-alpha"
		}
		return ""
	})
	keyB := sessionfp.FromHeader(func(h string) string {
		if strings.EqualFold(h, "X-Conversation-Id") {
			return "conv-beta"
		}
		return ""
	})
	if keyA == "" || keyB == "" || keyA == keyB {
		t.Fatalf("header keys not distinct: %q / %q", keyA, keyB)
	}

	// Both channel bindings must be recorded under the header-derived keys.
	chans := []string{model.ChannelTrae, model.ChannelWorkBuddy}
	found := 0
	for _, ch := range chans {
		_, okA := p.SessionBinding(ch, keyA)
		_, okB := p.SessionBinding(ch, keyB)
		if okA && okB {
			found++
		}
	}
	if found == 0 {
		t.Fatalf("no channel kept both session bindings (trae/wb) — the header key never reached the pool")
	}
}

// writeSSEAs answers with a minimal OpenAI-shaped stream.
func writeSSEAs(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+text+`"},"finish_reason":"stop"}]}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
