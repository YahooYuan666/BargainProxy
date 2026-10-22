package workbuddy

import (
	"bargainproxy/internal/prompt"
	"bargainproxy/internal/uperr"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// TestChannelRefusalRetriesWithSystemAsUser pins BP-93: a 400 code 11128
// ("Illegal API invocation from an unapproved channel") is the channel check
// refusing the caller's system prompt. The driver must send once more with
// that prompt moved into the first user turn inside <instructions> tags and
// the upstream's neutral system line first — and it must not penalize the
// account for it.
func TestChannelRefusalRetriesWithSystemAsUser(t *testing.T) {
	var calls int32
	var second map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&second)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	req := &ir.ChatRequest{
		Model:      "glm-5.3",
		SessionKey: "sess-1",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "You are Claude Code, Anthropic's official CLI."}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
		},
	}
	if _, err := d.Stream(context.Background(), driver.ChatCall{
		Account: testAccount(), UpstreamModel: "glm-5.3", Request: req,
	}, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original + moved retry)", got)
	}

	msgs, _ := second["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("second request has %d messages", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first message role = %v, want system (both builds require it)", first["role"])
	}
	if text, _ := first["content"].(string); text != prompt.DegradedSystemPrompt {
		t.Fatalf("first message = %q, want the neutral default", text)
	}
	// The refused identity line must now live in the user turn, wrapped.
	var user string
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m["role"] == "user" {
			user, _ = m["content"].(string)
			break
		}
	}
	if !strings.Contains(user, "<instructions>") || !strings.Contains(user, "Claude Code") {
		t.Fatalf("user turn does not carry the moved system prompt: %q", user)
	}
	if !strings.Contains(user, "hello") {
		t.Fatalf("the original user text was lost: %q", user)
	}
	// No system message may still hold the refused line.
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m["role"] == "system" && strings.Contains(fmt.Sprint(m["content"]), "Claude Code") {
			t.Fatalf("refused line still in a system message: %v", m["content"])
		}
	}
}

// TestChannelRefusalRetriesOnlyOnce pins that the move is a single retry: a
// second refusal is reported, not looped.
func TestChannelRefusalRetriesOnlyOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	req := &ir.ChatRequest{
		Model:      "glm-5.3",
		SessionKey: "sess-1",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "some agent line"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
	}
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account: testAccount(), UpstreamModel: "glm-5.3", Request: req,
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("want an error when the refusal stands after the move")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (never a loop)", got)
	}
	if !strings.Contains(err.Error(), "11128") {
		t.Fatalf("error should name the code so the panel can classify it: %v", err)
	}
}

// TestChannelRefusalIsNotAnAccountPenalty pins that the class is bad_request:
// the payload was refused, the account is fine.
func TestChannelRefusalIsNotAnAccountPenalty(t *testing.T) {
	ue := classifyBusiness(11128, "Illegal API invocation from an unapproved channel", "glm-5.3")
	if ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassBadRequest)
	}
}
