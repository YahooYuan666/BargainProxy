package antigravity

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-113: the upstream serves Claude under the BARE name (claude-sonnet-4-6),
// not the client display name (gemini-claude-sonnet-4-6). Field evidence
// 2026-10-25: the display name on the wire answered daily 404 "Requested
// entity was not found" and prod 429, while the bare name answered 200 for
// the same accounts (probed live); the IDE simultaneously showed 97% quota
// remaining, so neither error described the account — both described the
// unknown name.
func TestClientDisplayNameTranslatesToUpstreamBareName(t *testing.T) {
	cases := map[string]string{
		"gemini-claude-sonnet-4-6":        "claude-sonnet-4-6",
		"gemini-claude-opus-4-6-thinking": "claude-opus-4-6-thinking",
		// Already-bare names pass through untouched.
		"claude-sonnet-4-6": "claude-sonnet-4-6",
		// Gemini-family names are served under these very ids.
		"gemini-3-flash":        "gemini-3-flash",
		"gemini-3.8-flash-high": "gemini-3.8-flash-high",
	}
	for in, want := range cases {
		if got := upstreamModelName(in); got != want {
			t.Fatalf("upstreamModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The name that reaches the WIRE (inside the v1internal payload) must be the
// translated one, and the sig-replay cache stays keyed by the ORIGINAL routed
// name (call.UpstreamModel) — the translation is payload-only.
func TestWirePayloadCarriesTranslatedName(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-claude-sonnet-4-6", &ir.ChatRequest{
		Model:    "gemini-claude-sonnet-4-6",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Model != "claude-sonnet-4-6" {
		t.Fatalf("wire model = %q, want the bare upstream name (the display name earns a 404/429 from the upstream)", doc.Model)
	}
}
