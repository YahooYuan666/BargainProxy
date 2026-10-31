package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-227: WorkBuddy's 11128 channel check matches another agent's opener
// line WHOLE — one comma between the agent noun and its "powered by" phrase
// breaks the signature while keeping the system prompt a system prompt
// (plugins 79b02cc5 measured 400→200). The old handler jumped straight to
// moving the whole system into the user turn.
func TestBP227CommaRewritesTheOpener(t *testing.T) {
	cases := []struct{ in, want string }{
		{"You are an AI agent powered by DeepSeek Harness.", "You are an AI agent, powered by DeepSeek Harness."},
		{"You are an agent running on the acme runtime", "You are an agent, running on the acme runtime"},
		{"YOU ARE AN AI AGENT POWERED BY DEEPSEEK HARNESS", "YOU ARE AN AI AGENT, POWERED BY DEEPSEEK HARNESS"},
		{"You are the assistant based on nothing in particular", "You are the assistant, based on nothing in particular"},
		{"You are a helpful assistant", "You are a helpful assistant"}, // no phrase — untouched
		{"plain text", "plain text"},
	}
	for _, c := range cases {
		msgs := []map[string]any{{"role": "system", "content": c.in}}
		commaOpeners(msgs)
		if got := msgs[0]["content"]; got != c.want {
			t.Fatalf("commaOpeners(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The rewrite rides the WIRE: a live 11128 gets one comma retry, and a
// verified system is remembered so the NEXT request carries the comma from
// the start instead of paying the 400 again. The opener rides an ASSISTANT
// message — the shape the field evidence shows (the model quotes its agent
// opener back; the gateway replaces or strips system prompts).
func TestBP227RetryThenCache(t *testing.T) {
	var refusals atomic.Int32
	var openers []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		opener := ""
		for _, m := range body.Messages {
			if m.Role != "assistant" {
				continue
			}
			if s, ok := m.Content.(string); ok && strings.Contains(s, "powered by") {
				opener = s
			}
		}
		openers = append(openers, opener)
		if strings.Contains(opener, "powered by") && !strings.Contains(opener, ", powered by") {
			if refusals.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":11128,"msg":"illegal API invocation from an unapproved channel"}`)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	call := wbCall("glm-5.3", "")
	call.Account = testAccount()
	call.Request = &ir.ChatRequest{
		Model:      "glm-5.3",
		SessionKey: "sk-11128",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "keep the cache key non-trivial"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "You are an AI agent powered by DeepSeek Harness."}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
	}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("first stream: %v (the comma retry must clear the refusal)", err)
	}
	if refusals.Load() != 1 {
		t.Fatalf("refusals = %d, want exactly one 11128 before the comma cleared it", refusals.Load())
	}
	if len(openers) < 2 || !strings.Contains(openers[1], ", powered by") {
		t.Fatalf("the retry must carry the comma: %q", openers[len(openers)-1])
	}

	// A second request with the SAME system pre-applies the comma: no 400.
	call2 := wbCall("glm-5.3", "")
	call2.Account = call.Account
	call2.Request = call.Request
	if _, err := d.Stream(context.Background(), call2, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if refusals.Load() != 1 {
		t.Fatalf("refusals = %d, the cached comma must pre-apply (no second 400)", refusals.Load())
	}
	if last := openers[len(openers)-1]; !strings.Contains(last, ", powered by") {
		t.Fatalf("pre-applied comma missing: %q", last)
	}
}

// The move-system-to-user fallback still exists for openers the comma does
// not clear.
func TestBP227FallbackStillMovesSystem(t *testing.T) {
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 1<<16)
		n, _ := r.Body.Read(raw)
		bodies = append(bodies, string(raw[:n]))
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":11128,"msg":"illegal API invocation from an unapproved channel"}`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	call := wbCall("glm-5.3", "")
	call.Account = testAccount()
	call.Request = &ir.ChatRequest{
		Model:      "glm-5.3",
		SessionKey: "sk-11128b",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "some wholly different refusal trigger"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
	}
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("a persistent 11128 must surface after the fallback also fails")
	}
	// The opener shape is absent from the wire here (replaced/stripped
	// system), so the comma retry is SKIPPED — original post then the
	// systemAsUser fallback, no wasted round trip.
	if len(bodies) != 2 {
		t.Fatalf("chat posts = %d, want original then systemAsUser fallback", len(bodies))
	}
	if !strings.Contains(bodies[1], "some wholly different refusal trigger") {
		t.Fatalf("fallback body must carry the moved system: %.200s", bodies[1])
	}
}
