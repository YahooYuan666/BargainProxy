package sessionfp

import (
	"net/http"
	"testing"

	"bargainproxy/internal/ir"
)

// TestNativeHeaderBeatsContentDigest: two conversations that open with the
// same boilerplate must not share a key when the client names its own session
// (2026-10-20 cross-talk bug: ZCode's identical <system-reminder> prefix made
// two sessions collide).
func TestNativeHeaderBeatsContentDigest(t *testing.T) {
	sameOpening := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "<system-reminder>same boilerplate</system-reminder>\n继续"}}},
	}
	fromContent := FromMessages(sameOpening)
	if fromContent == "" {
		t.Skip("content fingerprint unavailable for this fixture")
	}
	// Both sessions look identical by content, so the content digest collides.
	if a, b := FromMessages(sameOpening), FromMessages(sameOpening); a != b {
		t.Fatalf("expected a content collision for this fixture: %q vs %q", a, b)
	}

	h1 := http.Header{}
	h1.Set("X-Conversation-Id", "session-one")
	h2 := http.Header{}
	h2.Set("X-Conversation-Id", "session-two")
	k1, k2 := FromHeader(h1.Get), FromHeader(h2.Get)
	if k1 == "" || k2 == "" {
		t.Fatal("native header must yield a key")
	}
	if k1 == k2 {
		t.Fatal("distinct native session ids must yield distinct keys")
	}
	if k1 == fromContent {
		t.Fatal("native key must not be the content digest")
	}
}

// TestNativeHeaderPrecedence: the first listed header wins, and non-listed
// headers are ignored, so a proxy-injected header cannot hijack routing.
func TestNativeHeaderPrecedence(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-Id", "second-choice")
	h.Set("X-BargainProxy-Session", "first-choice")
	if got := FromHeader(h.Get); got != digest("sid:", "first-choice") {
		t.Fatalf("precedence broken: %q", got)
	}
	other := http.Header{}
	other.Set("X-Random-Header", "nope")
	if got := FromHeader(other.Get); got != "" {
		t.Fatalf("unlisted header must be ignored, got %q", got)
	}
	// A blank or absurdly long value is not trusted.
	blank := http.Header{}
	blank.Set("X-Session-Id", "   ")
	if got := FromHeader(blank.Get); got != "" {
		t.Fatalf("blank value must be ignored, got %q", got)
	}
}

// TestNativeHeaderHashedNotRaw: the client's own id never appears verbatim, so
// it cannot leak into logs or upstream headers.
func TestNativeHeaderHashedNotRaw(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "raw-client-id-123")
	got := FromHeader(h.Get)
	if got == "" || got == "raw-client-id-123" {
		t.Fatalf("expected a hashed key, got %q", got)
	}
}
