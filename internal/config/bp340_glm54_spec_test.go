package config

import "testing"

// TestGLM54SpecMatchesTheCatalog pins the three tables GLM-5.4 needs.
//
// Why this test exists: glm-5.4 was routed and working but absent from every
// per-model table, so the gateway told clients it was a text-only model and an
// image request came back as "the current model does not support images"
// instead of being forwarded (field 2026-11-25, the same day it was added).
// Routing succeeding is not the same as the model being described correctly.
//
// The numbers are the MODEL's published figures from models.dev (synced
// 2026-11-23, release_date 2026-11-23), which is what "model capability" means
// here. AutoClaw's own catalog advertises larger numbers on its channel
// (context 1048576, maxTokens 307200) — that is a serving limit for one
// channel, not a property of the model, and declaring it would overstate both
// for every other channel and drop video/pdf from the accepted inputs.
//
//	models.dev: context 1000000, output 131072,
//	            input [text, image, video, pdf], effort [low,high,max], tools
func TestGLM54SpecMatchesTheCatalog(t *testing.T) {
	spec, ok := ModelSpecFor("glm-5.4", nil)
	if !ok {
		t.Fatal("glm-5.4 has no spec: clients will be told it cannot see images")
	}
	if spec.ContextLen != 1000000 {
		t.Errorf("ContextLen = %d, want 1000000 (models.dev; AutoClaw's 1048576 is channel-only)", spec.ContextLen)
	}
	if spec.MaxOutput != 131072 {
		t.Errorf("MaxOutput = %d, want 131072 (models.dev; AutoClaw's 307200 is channel-only)", spec.MaxOutput)
	}
	// All four modalities the model publishes. Declaring only text+image was
	// the original defect; dropping video/pdf would be the same mistake again.
	want := map[string]bool{"text": false, "image": false, "video": false, "pdf": false}
	for _, m := range spec.InputModes {
		want[m] = true
	}
	for mode, ok := range want {
		if !ok {
			t.Errorf("InputModes = %v is missing %q (models.dev lists text/image/video/pdf)", spec.InputModes, mode)
		}
	}
	if !spec.ToolCall {
		t.Error("ToolCall must be true: the catalog reports tool support")
	}
}

// TestGLM54ContextAndOutputTables: the other two tables that gate request
// shaping must agree with the spec, or a request is clamped to a number the
// model does not have.
func TestGLM54ContextAndOutputTables(t *testing.T) {
	if got, ok := ContextLength("glm-5.4"); !ok || got != 1000000 {
		t.Fatalf("ContextLength(glm-5.4) = %d, %v; want 1000000, true", got, ok)
	}
	if got, ok := OutputLimit("glm-5.4"); !ok || got != 131072 {
		t.Fatalf("OutputLimit(glm-5.4) = %d, %v; want 131072, true", got, ok)
	}
}

// TestGLM53FlashXLegalSpec: the model added alongside 5.4 had the same gap
// (catalog: ctx 307200, out 131072, ["image","text"]).
func TestGLM53FlashXLegalSpec(t *testing.T) {
	spec, ok := ModelSpecFor("glm-5.3-flashx-legal", nil)
	if !ok {
		t.Fatal("glm-5.3-flashx-legal has no spec")
	}
	if spec.ContextLen != 307200 || spec.MaxOutput != 131072 {
		t.Fatalf("ctx/out = %d/%d, want 307200/131072", spec.ContextLen, spec.MaxOutput)
	}
	hasImage := false
	for _, m := range spec.InputModes {
		if m == "image" {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatalf("InputModes = %v, want image included", spec.InputModes)
	}
}
