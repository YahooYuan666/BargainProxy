package prompt

import "testing"

// TestFixEnumTypesAddsMissingType: an enum with no type is exactly what MCP
// servers emit and what this upstream family rejects with 400 code 11133.
func TestFixEnumTypesAddsMissingType(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"enum": []any{"fast", "slow"}},
			"n":    map[string]any{"enum": []any{float64(1), float64(2)}},
			"flag": map[string]any{"enum": []any{true, false}},
		},
	}
	if !FixEnumTypes(params) {
		t.Fatal("expected a change")
	}
	props := params["properties"].(map[string]any)
	want := map[string]string{"mode": "string", "n": "number", "flag": "boolean"}
	for k, w := range want {
		got, _ := props[k].(map[string]any)["type"].(string)
		if got != w {
			t.Fatalf("%s type = %q, want %q", k, got, w)
		}
	}
	// Idempotent: a second pass changes nothing.
	if FixEnumTypes(params) {
		t.Fatal("second pass must be a no-op")
	}
}

// TestFixEnumTypesNarrowsMultiType: a multi-type property carrying an enum of
// one value kind is contradictory for the upstream, so it is narrowed.
func TestFixEnumTypesNarrowsMultiType(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level": map[string]any{"type": []any{"string", "null"}, "enum": []any{"low", "high"}},
		},
	}
	FixEnumTypes(params)
	got, _ := params["properties"].(map[string]any)["level"].(map[string]any)["type"].(string)
	if got != "string" {
		t.Fatalf("multi-type enum type = %v, want \"string\"", got)
	}
}

// TestFixEnumTypesNested: the repair reaches arrays of schemas and $defs, not
// just the top level (a fix that misses siblings is the classic half-fix).
func TestFixEnumTypesNested(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"list": map[string]any{
				"type":  "array",
				"items": map[string]any{"enum": []any{"a", "b"}},
			},
			"choice": map[string]any{
				"anyOf": []any{
					map[string]any{"enum": []any{"x"}},
					map[string]any{"type": "string"},
				},
			},
		},
		"$defs": map[string]any{
			"inner": map[string]any{"enum": []any{float64(7)}},
		},
	}
	FixEnumTypes(params)
	props := params["properties"].(map[string]any)
	items := props["list"].(map[string]any)["items"].(map[string]any)
	if items["type"] != "string" {
		t.Fatalf("items enum not typed: %v", items["type"])
	}
	branch := props["choice"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	if branch["type"] != "string" {
		t.Fatalf("anyOf branch enum not typed: %v", branch["type"])
	}
	def := params["$defs"].(map[string]any)["inner"].(map[string]any)
	if def["type"] != "number" {
		t.Fatalf("$defs enum not typed: %v", def["type"])
	}
}

// TestFixEnumTypesLeavesMixedAlone: a genuinely mixed enum cannot be typed
// without lying, so it is left for the upstream to complain about.
func TestFixEnumTypesLeavesMixedAlone(t *testing.T) {
	params := map[string]any{"enum": []any{"a", float64(1)}}
	if FixEnumTypes(params) {
		t.Fatal("mixed enum must not be typed")
	}
	if _, ok := params["type"]; ok {
		t.Fatal("mixed enum must stay untyped")
	}
}

// BP-128: a oneOf whose every branch is an object needs "type":"object" at
// the union level — Kimi 400s on an untyped object union (magpie workbuddy
// 0.1.33 / qoder 0.2.35).
func TestFixOneOfObjectUnionTyped(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
					map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "number"}}},
				},
			},
		},
	}
	if !FixEnumTypes(schema) {
		t.Fatal("expected a change")
	}
	target := schema["properties"].(map[string]any)["target"].(map[string]any)
	if target["type"] != "object" {
		t.Fatalf("union type = %v, want object", target["type"])
	}
}

// A $ref-only union counts as object-typed too; a mixed union does not get
// typed, and a schema already carrying a type is left alone.
func TestFixOneOfUnionEdges(t *testing.T) {
	refOnly := map[string]any{
		"oneOf": []any{
			map[string]any{"$ref": "#/$defs/a"},
			map[string]any{"$ref": "#/$defs/b"},
		},
	}
	if !FixEnumTypes(refOnly) || refOnly["type"] != "object" {
		t.Fatalf("ref-only union: type = %v, want object", refOnly["type"])
	}
	mixed := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "object"},
			map[string]any{"type": "string"},
		},
	}
	FixEnumTypes(mixed)
	if _, has := mixed["type"]; has {
		t.Fatalf("mixed union must stay untyped, got %v", mixed["type"])
	}
	typed := map[string]any{
		"type": "object",
		"oneOf": []any{
			map[string]any{"type": "object"},
			map[string]any{"type": "object"},
		},
	}
	FixEnumTypes(typed)
	if typed["type"] != "object" {
		t.Fatalf("existing type must be kept, got %v", typed["type"])
	}
}
