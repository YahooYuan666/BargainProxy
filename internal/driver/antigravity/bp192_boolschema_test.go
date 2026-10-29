package antigravity

import "testing"

// BP-192 (magpie 2cac98d, field case): MCP tools emit draft-2020-12 boolean
// schemas ("items": true) and the Google backend 400s on them. A bare true
// accepts anything (empty schema), a bare false matches nothing (the key
// carrying it is dropped).
func TestBP192BooleanSchemasAreNormalized(t *testing.T) {
	out := cleanGeminiSchema(map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"any":   true,
			"never": false,
			"named": map[string]any{"type": "STRING"},
		},
		"items": true,
	})
	m := out.(map[string]any)
	props := m["properties"].(map[string]any)
	if _, ok := props["never"]; ok {
		t.Fatal("a bare-false property can never match: it must be dropped")
	}
	if empty, ok := props["any"].(map[string]any); !ok || len(empty) != 0 {
		t.Fatalf("a bare-true property must become an empty schema, got %v", props["any"])
	}
	if named, ok := props["named"].(map[string]any); !ok || named["type"] != "STRING" {
		t.Fatalf("a normal property must survive untouched, got %v", props["named"])
	}
	if items, ok := m["items"].(map[string]any); !ok || len(items) != 0 {
		t.Fatalf("a bare-true items must become an empty schema, got %v", m["items"])
	}
}
