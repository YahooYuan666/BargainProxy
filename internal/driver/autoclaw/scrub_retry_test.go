package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// Test406RetryFallsBackToNeutralSystem pins the last-resort path against a
// server that answers 406 twice and then accepts.
//
// Since the production body is ALREADY scrubbed, a 406 means the refused claim
// named a product the marker list does not know. Resending the same body would
// earn the same 406, so the retry must change something structural: the system
// slot drops to a neutral line. The conversation itself is preserved.
func Test406RetryFallsBackToNeutralSystem(t *testing.T) {
	var calls int32
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if n == 1 {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{AppSecret: "s", UserAPIBase: srv.URL}, srv.Client())
	acc := &model.Account{
		ID:        "ac-1",
		Channel:   model.ChannelAutoClaw,
		CredsJSON: []byte(`{"access_token":"Bearer t","uid":"u","region":"cn"}`),
	}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{
				// An identity the marker list does not know: the scrub leaves it,
				// so the gateway refuses and the neutral fallback has to fire.
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
					Text: "You are SuperAgent 9000, a proprietary coding assistant."}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}

	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original + neutral retry)", got)
	}
	// The second body must be structurally different: a neutral system line and
	// no trace of the unknown identity.
	msgs, _ := bodies[1]["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("resent body has %d messages", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("resent first role = %q, want system", role)
	}
	sys, _ := first["content"].(string)
	if strings.Contains(sys, "SuperAgent 9000") {
		t.Fatalf("the refused identity was resent: %q", sys)
	}
	if !strings.Contains(sys, "helpful assistant") {
		t.Fatalf("the fallback did not use a neutral system line: %q", sys)
	}
	// The user turn survives untouched.
	second, _ := msgs[1].(map[string]any)
	if user, _ := second["content"].(string); user != "hi" {
		t.Fatalf("user turn = %q, want %q", user, "hi")
	}
}

// TestNo406MeansNoScrubRetry: a normal answer must not pay for a second call.
func TestNo406MeansNoScrubRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{AppSecret: "s", UserAPIBase: srv.URL}, srv.Client())
	acc := &model.Account{
		ID:        "ac-1",
		Channel:   model.ChannelAutoClaw,
		CredsJSON: []byte(`{"access_token":"Bearer t","uid":"u","region":"cn"}`),
	}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

// TestProductionBodyIsScrubbedUpFront pins that the FIRST request already
// carries the scrubbed prompt. Waiting for a 406 to learn what the prompt says
// costs a round trip on every single request, and it is the retry path that
// showed the intermittent ~30s dropped connection (2026-10-23).
func TestProductionBodyIsScrubbedUpFront(t *testing.T) {
	var calls int32
	var firstBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewDecoder(r.Body).Decode(&firstBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{AppSecret: "s", UserAPIBase: srv.URL}, srv.Client())
	acc := &model.Account{
		ID:        "ac-1",
		Channel:   model.ChannelAutoClaw,
		CredsJSON: []byte(`{"access_token":"Bearer t","uid":"u","region":"cn"}`),
	}
	const kept = "Prefer dedicated tools over shell."
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
					Text: "You are ZCode, a coding agent. " + kept}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the first body must already be scrubbed)", got)
	}
	msgs, _ := firstBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	first, _ := msgs[0].(map[string]any)
	sys, _ := first["content"].(string)
	if strings.Contains(strings.ToLower(sys), "you are zcode") {
		t.Fatalf("the refused claim went out on the first request: %q", sys)
	}
	if !strings.Contains(sys, kept) {
		t.Fatalf("the caller's instructions were lost: %q", sys)
	}
}

// TestProductionPathScrubsUpFront pins that the normal call path never sends a
// refused claim at all — the scrub is not a reaction to a 406 but part of
// building the body. Without this the first request of every ZCode
// conversation would pay a guaranteed refusal round trip.
func TestProductionPathScrubsUpFront(t *testing.T) {
	call := driver.ChatCall{
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
					Text: "You are Claude Code, Anthropic's official CLI. Be concise."}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
	raw, _, err := buildBodyScrubbed(call)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys, _ := doc.Messages[0].Content.(string)
	if strings.Contains(strings.ToLower(sys), "claude code") {
		t.Fatalf("the production body still carries the refused claim: %q", sys)
	}
	if !strings.Contains(sys, "Be concise.") {
		t.Fatalf("the production body dropped the caller's instructions: %q", sys)
	}
	// buildBody (the unscrubbed variant) must keep it: it is the escape hatch
	// tests and non-managed paths use.
	rawPlain, _, err := buildBody(call)
	if err != nil {
		t.Fatalf("build plain: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(rawPlain)), "claude code") {
		t.Fatal("buildBody should not scrub; it is the raw shape")
	}
}
