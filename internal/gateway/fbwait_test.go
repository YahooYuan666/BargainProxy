// Large contexts need more prefill time before the first token. The fixed 8s
// window cancelled a healthy workbuddy binding and cascaded to AutoClaw
// (2026-10-17 user report); the window now scales with estimated prompt size,
// capped per channel — AutoClaw's sandbox relay raises the cap so long ZCode
// sessions stop dying at exactly 30s (BP-71).
package gateway

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

func reqWithText(n int) *ir.ChatRequest {
	return &ir.ChatRequest{Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("x", n)}},
	}}}
}

func TestFBWaitFor(t *testing.T) {
	if got := fbWaitFor(8*time.Second, reqWithText(6), fbWindowCeiling); got != 8*time.Second {
		t.Fatalf("small prompt: got %s, want 8s", got)
	}
	// 240k chars ≈ 80k tokens → +10s.
	if got := fbWaitFor(8*time.Second, reqWithText(240000), fbWindowCeiling); got != 18*time.Second {
		t.Fatalf("80k tokens: got %s, want 18s", got)
	}
	// Extreme contexts cap at 30s.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), fbWindowCeiling); got != 30*time.Second {
		t.Fatalf("huge prompt: got %s, want 30s", got)
	}
	// Zero base falls back to the built-in 8s.
	if got := fbWaitFor(0, reqWithText(6), fbWindowCeiling); got != fallbackWait {
		t.Fatalf("zero base: got %s, want %s", got, fallbackWait)
	}
	// Zero ceiling falls back to the global cap too.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), 0); got != 30*time.Second {
		t.Fatalf("zero ceiling: got %s, want 30s", got)
	}
	// A raised ceiling (AutoClaw) lets long contexts keep waiting (BP-71)...
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), 90*time.Second); got != 90*time.Second {
		t.Fatalf("autoclaw ceiling: got %s, want 90s", got)
	}
	// ...but the formula still applies: 80k tokens only earns 18s.
	if got := fbWaitFor(8*time.Second, reqWithText(240000), 90*time.Second); got != 18*time.Second {
		t.Fatalf("autoclaw small: got %s, want 18s", got)
	}
}

// fbCeiling is per binding: AutoClaw gets the wide sandbox-relay ceiling
// (default 90s, configurable), everyone else keeps 30s (BP-71).
func TestFBCeilingPerChannel(t *testing.T) {
	g := &Gateway{cfg: config.Default()}
	if got := g.fbCeiling(model.ChannelWorkBuddy); got != fbWindowCeiling {
		t.Fatalf("workbuddy ceiling = %s, want 30s", got)
	}
	if got := g.fbCeiling(model.ChannelAutoClaw); got != defaultAutoClawFBWindow {
		t.Fatalf("autoclaw default ceiling = %s, want 90s", got)
	}
	g.cfg.Channels.AutoClaw.FirstByteWindow = config.Duration(120 * time.Second)
	if got := g.fbCeiling(model.ChannelAutoClaw); got != 120*time.Second {
		t.Fatalf("autoclaw configured ceiling = %s, want 120s", got)
	}
}
