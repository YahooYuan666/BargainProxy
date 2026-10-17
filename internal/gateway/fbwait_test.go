// Large contexts need more prefill time before the first token. The fixed 8s
// window cancelled a healthy workbuddy binding and cascaded to AutoClaw
// (2026-10-17 user report); the window now scales with estimated prompt size.
package gateway

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/ir"
)

func reqWithText(n int) *ir.ChatRequest {
	return &ir.ChatRequest{Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("x", n)}},
	}}}
}

func TestFBWaitFor(t *testing.T) {
	if got := fbWaitFor(8*time.Second, reqWithText(6)); got != 8*time.Second {
		t.Fatalf("small prompt: got %s, want 8s", got)
	}
	// 240k chars ≈ 80k tokens → +10s.
	if got := fbWaitFor(8*time.Second, reqWithText(240000)); got != 18*time.Second {
		t.Fatalf("80k tokens: got %s, want 18s", got)
	}
	// Extreme contexts cap at 30s.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000)); got != 30*time.Second {
		t.Fatalf("huge prompt: got %s, want 30s", got)
	}
	// Zero base falls back to the built-in 8s.
	if got := fbWaitFor(0, reqWithText(6)); got != fallbackWait {
		t.Fatalf("zero base: got %s, want %s", got, fallbackWait)
	}
}
