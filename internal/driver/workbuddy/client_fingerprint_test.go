package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// The shapes the official WorkBuddy desktop client sends. These are the whole
// point of BP-90: a gateway can send the right header *names* and still be
// identifiable by the *shapes* of their values.
var (
	uuidV4Re   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hex32Re    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex16Re    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	msPrefixRe = regexp.MustCompile(`^[0-9a-f]{12}`)
)

// TestChatSendsOfficialClientFingerprint pins the outbound identity: the
// official desktop client's header family, with values in the client's own
// shapes. Earlier revisions sent 32-hex conversation ids, the same string as
// both the per-message and the per-session id, and a trace id equal to the
// conversation id — each of those is a tell on its own (BP-90).
func TestChatSendsOfficialClientFingerprint(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	acc := testAccount()
	req := &ir.ChatRequest{
		Model:      "glm-5.3",
		SessionKey: "session-abc",
		Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	call := driver.ChatCall{Account: acc, UpstreamModel: "glm-5.3", Request: req}
	if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}

	// Values the client always sends, at their real values.
	want := map[string]string{
		"User-Agent":          desktopUA,
		"X-CodeBuddy-Request": "1",
		// The SKU segment, not the product name: the product lives in X-IDE-*.
		"X-Product":       "SaaS",
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   clientVersionCN,
		"X-Agent-Type":    "main",
		"X-Agent-Intent":  "craft",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Fatalf("%s = %q, want %q", k, got.Get(k), v)
		}
	}

	// Headers the desktop client never sends on chat. Extra headers are a
	// fingerprint too, and these came from the CodeBuddy CLI's conventions.
	for _, k := range []string{"Origin", "Referer", "Accept-Language",
		"X-Machine-ID", "X-Session-ID", "X-No-Enterprise-Id", "X-Requested-With"} {
		if v := got.Get(k); v != "" {
			t.Fatalf("%s = %q; the official client omits it on chat", k, v)
		}
	}

	// The SDK segment the client's openai-node runtime adds.
	for _, k := range []string{"X-Stainless-Lang", "X-Stainless-Package-Version",
		"X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Runtime",
		"X-Stainless-Runtime-Version", "X-Stainless-Retry-Count"} {
		if got.Get(k) == "" {
			t.Fatalf("%s must be present (client SDK segment)", k)
		}
	}

	// Id shapes.
	conv := got.Get("X-Conversation-ID")
	if !uuidV4Re.MatchString(conv) {
		t.Fatalf("X-Conversation-ID = %q, want a dashed UUID v4", conv)
	}
	convReq := got.Get("X-Conversation-Request-ID")
	if !uuidV4Re.MatchString(convReq) {
		t.Fatalf("X-Conversation-Request-ID = %q, want a dashed UUID v4", convReq)
	}
	if convReq == conv {
		t.Fatalf("conversation and turn ids are equal (%q); the client keeps them apart", conv)
	}
	if acp := got.Get("acp-connection-id"); !uuidV4Re.MatchString(acp) || acp == conv || acp == convReq {
		t.Fatalf("acp-connection-id = %q, want its own UUID v4", acp)
	}
	msg := got.Get("X-Request-ID")
	if !hex32Re.MatchString(msg) || !msPrefixRe.MatchString(msg) {
		t.Fatalf("X-Request-ID = %q, want 32 hex with a ms timestamp prefix", msg)
	}
	if got.Get("X-Conversation-Message-ID") != msg {
		t.Fatalf("X-Conversation-Message-ID must equal X-Request-ID")
	}
	trace := got.Get("X-Trace-ID")
	if !hex32Re.MatchString(trace) {
		t.Fatalf("X-Trace-ID = %q, want 32 hex", trace)
	}
	if trace == conv || trace == convReq || trace == msg {
		t.Fatalf("X-Trace-ID correlates with another id (%q); the client keeps it independent", trace)
	}
	if got.Get("X-B3-TraceId") != trace {
		t.Fatalf("X-B3-TraceId must follow X-Trace-ID")
	}
	span := got.Get("X-B3-SpanId")
	if !hex16Re.MatchString(span) {
		t.Fatalf("X-B3-SpanId = %q, want 16 hex", span)
	}
	if parent := got.Get("X-B3-ParentSpanId"); !hex16Re.MatchString(parent) {
		t.Fatalf("X-B3-ParentSpanId = %q, want 16 hex", parent)
	}
	// The client double-sends the single-header b3 and W3C traceparent forms;
	// only sending the X-B3-* trio is a shape it never produces.
	if b3 := got.Get("b3"); !strings.HasPrefix(b3, trace+"-"+span+"-1-") {
		t.Fatalf("b3 = %q, want %s-%s-1-<parent>", b3, trace, span)
	}
	if tp := got.Get("traceparent"); tp != "00-"+trace+"-"+span+"-01" {
		t.Fatalf("traceparent = %q, want 00-%s-%s-01", tp, trace, span)
	}

	// Same conversation: the stable ids repeat, the per-message ones don't.
	firstMsg := msg
	if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if got.Get("X-Conversation-ID") != conv {
		t.Fatalf("conversation id changed between turns: %q -> %q", conv, got.Get("X-Conversation-ID"))
	}
	if got.Get("X-Request-ID") == firstMsg {
		t.Fatalf("message id repeated across turns; each message is its own")
	}
}

// TestMessageIDsAreMillisecondOrdered pins the ordering property the client's
// ids have: within one conversation the leading 48 bits increase, so a burst
// does not look like a shuffle.
func TestMessageIDsAreMillisecondOrdered(t *testing.T) {
	prev := ""
	for i := 0; i < 50; i++ {
		id := newMessageID()
		if !hex32Re.MatchString(id) {
			t.Fatalf("id %q is not 32 hex", id)
		}
		if prev != "" && id[:12] < prev[:12] {
			t.Fatalf("id prefix went backwards: %q then %q", prev, id)
		}
		prev = id
	}
}

// TestCacheKeyIsHashedNotUID pins that the account uid does not appear in the
// outbound body: the client sends a UUID, and a plaintext uid is exactly what
// a risk-control rule would key on.
func TestCacheKeyIsHashedNotUID(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	acc := testAccount()
	req := &ir.ChatRequest{
		Model:      "glm-5.3",
		SessionKey: "session-abc",
		Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	if _, err := d.Stream(context.Background(), driver.ChatCall{
		Account: acc, UpstreamModel: "glm-5.3", Request: req,
	}, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, `"prompt_cache_key"`) {
		t.Fatalf("prompt_cache_key missing from body: %s", text)
	}
	// The test account's uid must not leak into the body.
	if uid := "u-100"; strings.Contains(text, uid) {
		t.Fatalf("account uid %q leaked into the request body", uid)
	}
	var doc struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !uuidV4Re.MatchString(doc.PromptCacheKey) {
		t.Fatalf("prompt_cache_key = %q, want a dashed UUID v4", doc.PromptCacheKey)
	}
}
