package media

import "testing"

// The catalog is the caller-facing contract: ids stable, params marked
// supported/unsupported, gemini-web present but browser-flagged (BP-306).
func TestCatalogShape(t *testing.T) {
	models := Catalog()
	if len(models) < 14 {
		t.Fatalf("catalog models = %d, want >= 14", len(models))
	}
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m.ID] {
			t.Fatalf("duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if len(m.Upstream) == 0 {
			t.Fatalf("%s: no upstream channels", m.ID)
		}
		if m.Kind != "image" && m.Kind != "video" {
			t.Fatalf("%s: kind %q", m.ID, m.Kind)
		}
	}
	if !seen["nano-banana-2"] || !seen["hy-image-3.5"] || !seen["minimax-video-h3"] || !seen["veo-3-1"] {
		t.Fatalf("expected key models missing: %v", seen)
	}
	veo, _ := ByID("veo-3-1")
	if !veo.Browser {
		t.Fatal("veo-3-1 must carry requires_browser")
	}
	if _, ok := ByID("no-such-model"); ok {
		t.Fatal("ByID should miss unknown ids")
	}
}

// Capability lookups drive the switch-model error (BP-306 §4.1).
func TestCapabilityModels(t *testing.T) {
	can := CapabilityModels("image", "aspect_ratio")
	if len(can) == 0 {
		t.Fatal("no image model declares aspect_ratio")
	}
	for _, id := range can {
		if id == "qwen-image-3" {
			t.Fatal("qwen-image-3 must NOT declare aspect_ratio")
		}
		if id == "seedream-5-pro" {
			t.Fatal("seedream-5-pro must NOT declare aspect_ratio (vendor ignores it, 2026-11-13 probe)")
		}
	}
}

// Route resolution: preferred channel wins when served, gemini-web never
// selected while unwired, unknown preferred falls back to first served.
func TestRoute(t *testing.T) {
	nb, _ := ByID("nano-banana-2")
	if ch, up, ok := Route(nb, ""); !ok || ch != "antigravity" || up != "gemini-3.1-flash-image" {
		t.Fatalf("nano-banana-2 default route = %s/%s ok=%v", ch, up, ok)
	}
	mm, _ := ByID("minimax-video-h3")
	if ch, _, ok := Route(mm, "lobster"); !ok || ch != "lobster" {
		t.Fatalf("preferred lobster route = %s ok=%v", ch, ok)
	}
	if ch, _, ok := Route(mm, "gemini-web"); !ok || ch == "gemini-web" {
		t.Fatalf("unwired channel selected: %s ok=%v", ch, ok)
	}
	hy, _ := ByID("hy-image-3.5")
	if ch, up, ok := Route(hy, ""); !ok || ch != "workbuddy" || up != "hy-image-3.5" {
		t.Fatalf("hy route = %s/%s ok=%v", ch, up, ok)
	}
}

// One name, one entry: the base/extra override must never leave contradictory
// duplicates — get() reads the first, CapabilityModels reads any Supported
// entry, and a dup makes the two disagree (sess_4041 400 root cause).
func TestCatalogNoDuplicateParams(t *testing.T) {
	for _, m := range Catalog() {
		seen := map[string]bool{}
		for _, p := range m.Params {
			if seen[p.Name] {
				t.Fatalf("%s: duplicate param %q", m.ID, p.Name)
			}
			seen[p.Name] = true
		}
	}
}
