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

// TestConversationIDNeverSharedWithoutSessionKey pins the 2026-10-20 fix:
// when the client gives no stable session identity, each request must get its
// OWN conversation id. A constant fallback told upstream that unrelated chats
// were one conversation, and the model's answers/images crossed over between
// them. With a session key the id must stay stable (one conversation).
func TestConversationIDNeverSharedWithoutSessionKey(t *testing.T) {
	seen := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Conversation-ID")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	acc := testAccount()
	send := func(sessionKey string) string {
		req := &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		}
		req.SessionKey = sessionKey
		if _, err := d.Stream(context.Background(), driver.ChatCall{
			Account: acc, UpstreamModel: "glm-5.3", Request: req,
		}, func(ev ir.StreamEvent) error { return nil }); err != nil {
			t.Fatalf("stream: %v", err)
		}
		return <-seen
	}

	// No session key: two requests must not share a conversation id.
	a, b := send(""), send("")
	if a == "" || b == "" {
		t.Fatalf("conversation id missing: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("two unrelated requests shared conversation id %q", a)
	}

	// With a session key: stable across turns of one conversation.
	c, e := send("sess-1"), send("sess-1")
	if c != e {
		t.Fatalf("same session must keep one conversation id: %q vs %q", c, e)
	}
}
