package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// TestChatSendsOfficialClientFingerprint pins the 2026-10-20 anti-risk-control
// change: chat requests carry the official desktop client's header family.
// The chat edge classifies traffic, and accounts were being restricted
// (403 code=11140 "request illegal" / 账号存在异常行为) after the gateway had
// been sending bare four-header requests. Values must be STABLE per
// account/session — a random id per request looks worse than none.
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
	if _, err := d.Stream(context.Background(), driver.ChatCall{
		Account: acc, UpstreamModel: "glm-5.3", Request: req,
	}, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}

	want := map[string]string{
		"User-Agent":          desktopUA,
		"X-CodeBuddy-Request": "1",
		"X-Product":           "WorkBuddy",
		"X-Agent-Purpose":     "conversation",
		"X-IDE-Name":          "WorkBuddy",
		"Accept-Language":     "zh-CN",
		"Origin":              "https://www.codebuddy.cn",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Fatalf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
	for _, k := range []string{"X-User-Id", "X-Conversation-ID", "X-Conversation-Request-ID",
		"X-Conversation-Message-ID", "X-Request-ID", "X-Root-Request-ID", "X-Trace-ID",
		"X-B3-TraceId", "X-B3-SpanId", "X-Machine-ID", "X-Session-ID"} {
		if got.Get(k) == "" {
			t.Fatalf("%s must be present (official header family)", k)
		}
	}
	// Same session must produce the same conversation id on the next call.
	conv := got.Get("X-Conversation-ID")
	if _, err := d.Stream(context.Background(), driver.ChatCall{
		Account: acc, UpstreamModel: "glm-5.3", Request: req,
	}, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if got.Get("X-Conversation-ID") != conv {
		t.Fatalf("conversation id changed between calls: %q -> %q", conv, got.Get("X-Conversation-ID"))
	}
}
