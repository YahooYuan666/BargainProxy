// Large contexts need more prefill time before the first token. The fixed 8s
// window cancelled a healthy workbuddy binding and cascaded to AutoClaw
// (2026-10-17 user report); the window now scales with estimated prompt size.
// BP-72 made the ceiling global (90s, configurable per channel) after
// WorkBuddy kept dying at the old 30s cap on 300k-token ZCode sessions.
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
	// 240k chars ≈ 120k estimated tokens → 8s + 60s = 68s, capped at the
	// global 60s ceiling (BP-118). The cap still binds — it is what keeps a
	// five-leg cascade bounded — it is just no longer below the prefill time
	// of the conversations this gateway actually serves.
	if got := fbWaitFor(8*time.Second, reqWithText(240000), fbWindowCeiling); got != 60*time.Second {
		t.Fatalf("120k tokens: got %s, want the 60s ceiling", got)
	}
	// Extreme contexts cap at the global ceiling (BP-118 raised it 30s → 60s:
	// a 33万-token prefill measured 31.9s, so the 30s cap was cancelling
	// requests that would have answered).
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), fbWindowCeiling); got != 60*time.Second {
		t.Fatalf("huge prompt: got %s, want 60s", got)
	}
	// A channel with its own ceiling (AutoClaw's relay needs minutes) is not
	// capped by the global one.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), 90*time.Second); got != 90*time.Second {
		t.Fatalf("per-channel ceiling: got %s, want 90s", got)
	}
	// Zero base falls back to the built-in 8s.
	if got := fbWaitFor(0, reqWithText(6), fbWindowCeiling); got != fallbackWait {
		t.Fatalf("zero base: got %s, want %s", got, fallbackWait)
	}
	// Zero ceiling falls back to the global cap too.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), 0); got != 60*time.Second {
		t.Fatalf("zero ceiling: got %s, want 60s", got)
	}
	// A raised per-channel ceiling lets long contexts keep waiting...
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), 180*time.Second); got != 180*time.Second {
		t.Fatalf("raised ceiling: got %s, want 180s", got)
	}
	// ...but the formula still applies: 120k estimated tokens earn 68s.
	if got := fbWaitFor(8*time.Second, reqWithText(240000), 180*time.Second); got != 68*time.Second {
		t.Fatalf("small under raised ceiling: got %s, want 68s", got)
	}
}

// Tool schemas count toward the estimate: ZCode ships a large tool block and
// the old text-only estimate kept the window at its floor.
func TestFBBudgetCountsToolsAndImages(t *testing.T) {
	req := reqWithText(3)
	req.Tools = []ir.ToolDef{{
		Name:        "read_file",
		Description: strings.Repeat("d", 12000),
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}}
	if got := estimatePromptTokens(req); got < 4000 {
		t.Fatalf("tool schema not counted: est=%d", got)
	}
	img := &ir.ChatRequest{Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.ContentPart{{Kind: "image", ImageMIME: "image/png", ImageB64: strings.Repeat("A", 1000)}},
	}}}
	if got := estimatePromptTokens(img); got != 1500 {
		t.Fatalf("image allowance: got %d, want 1500", got)
	}
}

// fbCeiling resolution: per-channel override → legacy AutoClaw field →
// fbWindowCeiling. BP-138 removed the panel value from this chain: it is now
// the BASE of the dynamic formula (see TestFBDynamicWindowGrowsWithContext),
// not a cap — its old place here is what kept clamping big prompts back to 30s.
func TestFBCeilingPerChannel(t *testing.T) {
	g := &Gateway{cfg: config.Default()}
	for _, ch := range []string{model.ChannelWorkBuddy, model.ChannelAutoClaw, model.ChannelTrae, model.ChannelQoder} {
		if got := g.fbCeiling(ch); got != fbWindowCeiling {
			t.Fatalf("%s default ceiling = %s, want %s", ch, got, fbWindowCeiling)
		}
	}

	g.cfg.ChannelFirstByteWindows = map[string]config.Duration{
		model.ChannelWorkBuddy: config.Duration(180 * time.Second),
	}
	if got := g.fbCeiling(model.ChannelWorkBuddy); got != 180*time.Second {
		t.Fatalf("workbuddy override = %s, want 180s", got)
	}
	if got := g.fbCeiling(model.ChannelAutoClaw); got != fbWindowCeiling {
		t.Fatalf("autoclaw should follow the default = %s", got)
	}

	// Legacy BP-71 field still wins for AutoClaw.
	g.cfg.Channels.AutoClaw.FirstByteWindow = config.Duration(240 * time.Second)
	if got := g.fbCeiling(model.ChannelAutoClaw); got != 240*time.Second {
		t.Fatalf("legacy autoclaw ceiling = %s, want 240s", got)
	}

	// BP-138: the panel value must NOT act as a ceiling any more.
	g.SetFirstByteWindow(30 * time.Second)
	if got := g.fbCeiling(model.ChannelQoder); got != fbWindowCeiling {
		t.Fatalf("panel value leaked into the ceiling: %s (it is the BASE now)", got)
	}
	if got := g.fbBaseFor(); got != 30*time.Second {
		t.Fatalf("panel value must be the base: %s, want 30s", got)
	}
	g.SetFirstByteWindow(0)
	if got := g.fbBaseFor(); got == 30*time.Second {
		t.Fatal("clearing the panel value must fall back to the config default")
	}
}

// TestFBDynamicWindowGrowsWithContext is the BP-138 regression test.
//
// Field 2026-10-27 20:20:29: a 56.8万-token prompt went workbuddy → trae →
// qoder → lobster and died on all four. The workbuddy leg was cancelled at
// exactly 30.000s because the DB panel value 30 was the CAP; the measured
// prefill rate (33万 tokens = 31.9s, BP-118) says that prompt needed ~50s.
func TestFBDynamicWindowGrowsWithContext(t *testing.T) {
	g := &Gateway{cfg: config.Default()}
	g.SetFirstByteWindow(30 * time.Second) // the operator's base, as stored

	small := &ir.ChatRequest{Messages: []ir.Message{{
		Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 4000)}},
	}}}
	big := &ir.ChatRequest{Messages: []ir.Message{{
		Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 1_120_000)}},
	}}}

	smallWin := fbWaitFor(g.fbBaseFor(), small, g.fbCeiling(model.ChannelWorkBuddy))
	bigWin := fbWaitFor(g.fbBaseFor(), big, g.fbCeiling(model.ChannelWorkBuddy))

	// A ~2k-token prompt keeps the operator's base (plus at most a second).
	if smallWin < 30*time.Second || smallWin > 32*time.Second {
		t.Fatalf("small prompt window = %s, want the base 30-32s", smallWin)
	}
	// 56万 tokens needs ~280s at 2k/s, so the window must reach the ceiling —
	// NOT stop at the operator's 30s base.
	if bigWin != g.fbCeiling(model.ChannelWorkBuddy) {
		t.Fatalf("560k-token window = %s, want the %s ceiling (the base must not cap it)", bigWin, g.fbCeiling(model.ChannelWorkBuddy))
	}
	if bigWin <= 30*time.Second {
		t.Fatalf("560k-token window = %s: the fixed 30s cap is back", bigWin)
	}
}
