package antigravity

import (
	"encoding/json"
	"testing"
)

// TestCleanGeminiSchema: the endpoint rejects OpenAI-style JSON Schemas with
// 400 "Unknown name ..." (observed live: "schema" keys inside parameters).
// The cleaner must keep only the Gemini subset, upper-case types, collapse
// ["string","null"] to type+nullable, coerce enums to strings and recurse.
func TestCleanGeminiSchema(t *testing.T) {
	in := map[string]any{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"additionalProperties": false,
		"type":                 "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type": []any{"string", "null"},
				"enum": []any{"auto", "manual", 42},
			},
			"limit": map[string]any{
				"type": "integer", // OpenAI-style lowercase
			},
			"tags": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type":        "string",
					"minLength":   2,
					"description": "a tag",
				},
			},
			"spec": map[string]any{ // the live-observed offender
				"schema": map[string]any{"type": "object"},
			},
		},
		"required": []any{"mode"},
	}
	out := cleanGeminiSchema(in)
	b, _ := json.Marshal(out)
	s := string(b)

	for _, banned := range []string{"$schema", "additionalProperties", "minItems", "minLength", `"schema"`} {
		if contains(s, banned) {
			t.Fatalf("banned key survived: %s in %s", banned, s)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "OBJECT" {
		t.Fatalf("type = %v, want OBJECT (upper-cased)", m["type"])
	}
	props := m["properties"].(map[string]any)
	mode := props["mode"].(map[string]any)
	if mode["type"] != "STRING" || mode["nullable"] != true {
		t.Fatalf("mode = %v, want STRING+nullable", mode)
	}
	if mode["enum"] == nil {
		t.Fatal("enum dropped entirely; want string members kept")
	}
	limit := props["limit"].(map[string]any)
	if limit["type"] != "INTEGER" {
		t.Fatalf("limit type = %v, want INTEGER", limit["type"])
	}
	tags := props["tags"].(map[string]any)
	if tags["type"] != "ARRAY" {
		t.Fatalf("tags type = %v", tags["type"])
	}
	items := tags["items"].(map[string]any)
	if items["type"] != "STRING" || items["description"] != "a tag" {
		t.Fatalf("items = %v", items)
	}
	spec, ok := props["spec"].(map[string]any)
	if ok && len(spec) != 0 {
		// "spec" itself is a valid key name, but its "schema" child must be
		// stripped, leaving an empty object.
		t.Fatalf("spec.schema must be stripped, got %v", spec)
	}
	if req, ok := m["required"].([]any); !ok || len(req) != 1 || req[0] != "mode" {
		t.Fatalf("required = %v", m["required"])
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
