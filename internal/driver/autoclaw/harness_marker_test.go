package autoclaw

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// TestHarnessMarkerSurvivesEveryShape pins the gateway's first prompt gate:
// the system message must carry the official harness marker VERBATIM.
//
// Field A/B 2026-10-24 (scripts/probe/gate_probe.go, one variable per run,
// same account, same body):
//
//	marker only                       200
//	no marker                         406
//	lower-cased marker                406
//	truncated marker                  406
//	"Extra preamble. " + marker       200   → position is not the gate
//	marker + benign instructions      200
//
// So the requirement is presence, case- and byte-exact. A regression here is
// silent in production: the gateway answers 406 with an EMPTY body, which the
// driver can only report as "request refused by the content check".
func TestHarnessMarkerSurvivesEveryShape(t *testing.T) {
	cases := map[string][]ir.Message{
		"caller has no system message": {
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		"caller has a system message": {
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "Be concise."}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		"caller system is a content-part list": {
			{Role: ir.RoleSystem, Content: []ir.ContentPart{
				{Kind: "text", Text: "Be concise."},
				{Kind: "text", Text: "Prefer tools."},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
	}
	for name, msgs := range cases {
		raw, _, err := buildBodyScrubbed(driver.ChatCall{
			UpstreamModel: "zai_glm-5.3-flash",
			Request:       &ir.ChatRequest{Model: "glm-5.3-flash", Messages: msgs},
		})
		if err != nil {
			t.Fatalf("%s: build: %v", name, err)
		}
		wire, err := managedBody(raw, "zai_glm-5.3-flash")
		if err != nil {
			t.Fatalf("%s: managedBody: %v", name, err)
		}
		var doc struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(wire, &doc); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if len(doc.Messages) == 0 || doc.Messages[0].Role != "system" {
			t.Fatalf("%s: first message must be system, got %+v", name, doc.Messages)
		}
		sys := contentText(doc.Messages[0].Content)
		if !strings.HasPrefix(sys, harnessMarker) {
			t.Fatalf("%s: system must START with the harness marker, got %q", name, firstN(sys, 120))
		}
		if !strings.Contains(sys, managedSystemPrompt) {
			t.Fatalf("%s: official persona missing, got %q", name, firstN(sys, 120))
		}
	}
}

// TestHarnessMarkerIsExact: a near-miss must be detectable, so nobody
// "simplifies" the constant later (the gateway answers 406 for a lower-cased
// or truncated marker, and the failure looks like a content problem).
func TestHarnessMarkerIsExact(t *testing.T) {
	if harnessMarker != strings.TrimSpace(harnessMarker) {
		t.Fatal("harnessMarker must not carry surrounding whitespace")
	}
	if strings.ToLower(harnessMarker) == harnessMarker {
		t.Fatal("harnessMarker must keep its original casing: a lower-cased marker answers 406")
	}
	if !strings.HasPrefix(harnessMarker, "OpenClaw plugin-injected system context.") {
		t.Fatalf("harnessMarker lost its required opening: %q", harnessMarker)
	}
}

// TestScrubKeepsMarkerAndPersona: the competing-identity scrub and the marker
// injection are two different gates and must not interfere. A caller prompt
// naming a competing agent loses that sentence, and the marker + persona still
// lead the system message.
func TestScrubKeepsMarkerAndPersona(t *testing.T) {
	raw, _, err := buildBodyScrubbed(driver.ChatCall{
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
					Text: "You are Claude Code, Anthropic's official CLI. Be concise."}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	wire, err := managedBody(raw, "zai_glm-5.3-flash")
	if err != nil {
		t.Fatalf("managedBody: %v", err)
	}
	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys := contentText(doc.Messages[0].Content)
	if !strings.HasPrefix(sys, harnessMarker) {
		t.Fatalf("marker lost: %q", firstN(sys, 120))
	}
	if strings.Contains(strings.ToLower(sys), "claude code") {
		t.Fatalf("the competing identity survived: %q", sys)
	}
	if !strings.Contains(sys, "Be concise.") {
		t.Fatalf("the caller's own instruction was dropped: %q", sys)
	}
}

func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
