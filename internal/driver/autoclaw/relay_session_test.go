package autoclaw

import (
	"testing"

	"bargainproxy/internal/ir"
)

// TestRelaySessionKeyNeverShared pins the 2026-10-20 fix: two requests without
// a stable session identity must never share one sandbox agent session. The
// old "main" fallback pooled every conversation into one agent context, which
// is how answers and images from one chat appeared in another.
func TestRelaySessionKeyNeverShared(t *testing.T) {
	a := relaySessionKey(&ir.ChatRequest{})
	b := relaySessionKey(&ir.ChatRequest{})
	if a == "" || b == "" {
		t.Fatalf("keys must not be empty: %q %q", a, b)
	}
	if a == "main" || b == "main" {
		t.Fatal(`the shared "main" fallback must be gone`)
	}
	if a == b {
		t.Fatalf("two unrelated requests shared sandbox session %q", a)
	}

	// A stable client session is forwarded as-is (per-conversation continuity).
	if got := relaySessionKey(&ir.ChatRequest{SessionKey: "sess-42"}); got != "sess-42" {
		t.Fatalf("session key = %q, want sess-42", got)
	}
}
