package prompt

// BP-331 (misakano 733942a7, 2026-11-18 per-shape field matrix): keyword
// VALUES are deserialized with Go strong typing and a wrong type is a
// whole-request 400 (11129); a TOP-LEVEL $ref is rejected outright while
// nested refs answer 200.

import "testing"

func TestBP331CoerceNumberKeys(t *testing.T) {
	s := map[string]any{"minimum": "1", "maximum": float64(9), "multipleOf": "abc"}
	changed := CoerceSchemaKeywordTypes(s)
	if !changed {
		t.Fatal("coercion must report changes")
	}
	if v, _ := s["minimum"].(float64); v != 1 {
		t.Fatalf("minimum = %v (%T), want 1 (number)", s["minimum"], s["minimum"])
	}
	if v, _ := s["maximum"].(float64); v != 9 {
		t.Fatalf("maximum = %v, want untouched number", s["maximum"])
	}
	if _, ok := s["multipleOf"]; ok {
		t.Fatal("an uncoercible keyword must be DROPPED (keeping it guarantees a 400)")
	}
}

func TestBP331CoerceBoolAndStringKeys(t *testing.T) {
	s := map[string]any{
		"uniqueItems":          "true",
		"deprecated":           false,
		"description":          123,
		"title":                "T",
		"additionalProperties": "false",
	}
	CoerceSchemaKeywordTypes(s)
	if v, _ := s["uniqueItems"].(bool); !v {
		t.Fatalf("uniqueItems = %v (%T), want true (bool)", s["uniqueItems"], s["uniqueItems"])
	}
	if v, _ := s["deprecated"].(bool); v {
		t.Fatal("bool value must stay")
	}
	if _, ok := s["description"]; ok {
		t.Fatal("numeric description must be dropped")
	}
	if v, _ := s["additionalProperties"].(bool); v {
		t.Fatal("additionalProperties \"false\" must become a real false")
	}
	if _, ok := s["title"]; !ok {
		t.Fatal("string title must stay")
	}
}

func TestBP331CoerceEnumExamplesRequired(t *testing.T) {
	s := map[string]any{"enum": "x", "examples": 7, "required": []any{"a", "a", " ", 3, "b"}}
	CoerceSchemaKeywordTypes(s)
	if _, ok := s["enum"]; ok {
		t.Fatal("a non-array enum must be dropped")
	}
	if _, ok := s["examples"]; ok {
		t.Fatal("a non-array examples must be dropped")
	}
	req, _ := s["required"].([]any)
	if len(req) != 2 || req[0] != "a" || req[1] != "b" {
		t.Fatalf("required = %v, want [a b] (dup/empty/non-string entries dropped)", req)
	}
	// An emptied required list disappears instead of sending [].
	s2 := map[string]any{"required": []any{"  ", 1}}
	CoerceSchemaKeywordTypes(s2)
	if _, ok := s2["required"]; ok {
		t.Fatal("an emptied required must be dropped")
	}
}

func TestBP331CoerceRecurses(t *testing.T) {
	s := map[string]any{
		"properties": map[string]any{
			"n": map[string]any{"type": "integer", "minimum": "1"},
		},
		"items": map[string]any{"uniqueItems": "true"},
	}
	changed := CoerceSchemaKeywordTypes(s)
	if !changed {
		t.Fatal("nested changes must be reported")
	}
	props := s["properties"].(map[string]any)["n"].(map[string]any)
	if v, _ := props["minimum"].(float64); v != 1 {
		t.Fatalf("nested minimum = %v, want 1", props["minimum"])
	}
	items := s["items"].(map[string]any)
	if v, _ := items["uniqueItems"].(bool); !v {
		t.Fatalf("nested uniqueItems = %v, want bool", items["uniqueItems"])
	}
}

// Top-level $ref: expandable from $defs → inlined; not → the ref is dropped
// (a tool that can never be called is worse than one called without args).
func TestBP331TopLevelRef(t *testing.T) {
	withDefs := map[string]any{
		"$ref":  "#/$defs/Foo",
		"$defs": map[string]any{"Foo": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
	}
	if !FixTopLevelRef(withDefs) {
		t.Fatal("a present $ref must report a change")
	}
	if _, ok := withDefs["$ref"]; ok {
		t.Fatal("$ref must be gone")
	}
	props, _ := withDefs["properties"].(map[string]any)
	if _, ok := props["x"]; !ok {
		t.Fatalf("the real definition must be inlined: %v", withDefs)
	}

	bare := map[string]any{"$ref": "#/$defs/Missing"}
	if !FixTopLevelRef(bare) {
		t.Fatal("dropping an unresolvable ref is still a change")
	}
	if _, ok := bare["$ref"]; ok {
		t.Fatal("an unresolvable $ref must be dropped, not kept (keeping it guarantees a 400)")
	}

	plain := map[string]any{"type": "object"}
	if FixTopLevelRef(plain) {
		t.Fatal("no $ref → no change")
	}

	// "#/properties" must NOT falsely resolve against the whole schema —
	// only the $defs/definitions table is consulted.
	tricky := map[string]any{"$ref": "#/properties", "properties": map[string]any{"a": map[string]any{}}}
	FixTopLevelRef(tricky)
	if _, ok := tricky["properties"]; !ok {
		t.Fatal("an unresolvable pointer must not clobber the schema")
	}
}
