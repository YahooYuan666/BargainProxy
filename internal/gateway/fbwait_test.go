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
	// 240k chars ≈ 120k estimated tokens → +60s (1s per 2k, conservative
	// chars/2 estimate so CJK prompts are never under-counted).
	if got := fbWaitFor(8*time.Second, reqWithText(240000), fbWindowCeiling); got != 68*time.Second {
		t.Fatalf("120k tokens: got %s, want 68s", got)
	}
	// Extreme contexts cap at the global ceiling.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), fbWindowCeiling); got != 90*time.Second {
		t.Fatalf("huge prompt: got %s, want 90s", got)
	}
	// Zero base falls back to the built-in 8s.
	if got := fbWaitFor(0, reqWithText(6), fbWindowCeiling); got != fallbackWait {
		t.Fatalf("zero base: got %s, want %s", got, fallbackWait)
	}
	// Zero ceiling falls back to the global cap too.
	if got := fbWaitFor(8*time.Second, reqWithText(10_000_000), 0); got != 90*time.Second {
		t.Fatalf("zero ceiling: got %s, want 90s", got)
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
// global setting → 90s default (BP-72).
func TestFBCeilingPerChannel(t *testing.T) {
	g := &Gateway{cfg: config.Default()}
	for _, ch := range []string{model.ChannelWorkBuddy, model.ChannelAutoClaw, model.ChannelTrae, model.ChannelQoder} {
		if got := g.fbCeiling(ch); got != fbWindowCeiling {
			t.Fatalf("%s default ceiling = %s, want 90s", ch, got)
		}
	}

	g.cfg.FirstByteWindow = config.Duration(120 * time.Second)
	if got := g.fbCeiling(model.ChannelWorkBuddy); got != 120*time.Second {
		t.Fatalf("global ceiling = %s, want 120s", got)
	}

	g.cfg.ChannelFirstByteWindows = map[string]config.Duration{
		model.ChannelWorkBuddy: config.Duration(180 * time.Second),
	}
	if got := g.fbCeiling(model.ChannelWorkBuddy); got != 180*time.Second {
		t.Fatalf("workbuddy override = %s, want 180s", got)
	}
	if got := g.fbCeiling(model.ChannelAutoClaw); got != 120*time.Second {
		t.Fatalf("autoclaw should follow global = %s, want 120s", got)
	}

	// Legacy BP-71 field still wins over the global setting for AutoClaw.
	g.cfg.Channels.AutoClaw.FirstByteWindow = config.Duration(240 * time.Second)
	if got := g.fbCeiling(model.ChannelAutoClaw); got != 240*time.Second {
		t.Fatalf("legacy autoclaw ceiling = %s, want 240s", got)
	}

	// Panel hot setting wins over the config global but not per-channel map.
	g.SetFirstByteWindow(150 * time.Second)
	if got := g.fbCeiling(model.ChannelQoder); got != 150*time.Second {
		t.Fatalf("hot global ceiling = %s, want 150s", got)
	}
	if got := g.fbCeiling(model.ChannelWorkBuddy); got != 180*time.Second {
		t.Fatalf("channel override must survive the hot global = %s, want 180s", got)
	}
	g.SetFirstByteWindow(0)
	if got := g.fbCeiling(model.ChannelQoder); got != 120*time.Second {
		t.Fatalf("cleared hot ceiling = %s, want the config global 120s", got)
	}
}
