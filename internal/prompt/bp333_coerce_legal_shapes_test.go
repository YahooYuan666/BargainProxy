package prompt

// BP-333: CoerceSchemaKeywordTypes must never compare an `any` value with ==.
//
// The first version asked `if nv != v` to decide whether to report a change,
// and `v` is whatever the client sent. Go cannot compare maps or slices with
// ==, so the first enum array and the first additionalProperties OBJECT
// panicked the request goroutine with
//
//	runtime error: comparing uncomparable type map[string]interface {}
//
// Those two are exactly the shapes the upstream field matrix lists as LEGAL
// (200): they must be passed through untouched, not repaired. The upstream
// implementation this was ported from reports no change value at all, so it
// never had the comparison to get wrong — the bug was introduced by the local
// addition of a bool return, not by the ported logic.
//
// Every case below crashed before the fix. The scalar-repair cases live in
// bp331_schema_test.go; this file is the other half of the matrix: the legal
// shapes.

import "testing"

// A legal enum array must survive byte-identical and report no change. This is
// the exact shape that panicked (`[]interface{}` is uncomparable).
func TestBP333LegalEnumArrayIsUntouched(t *testing.T) {
	s := map[string]any{
		"type": "string",
		"enum": []any{"fast", "slow"},
	}
	if changed := CoerceSchemaKeywordTypes(s); changed {
		t.Fatal("a legal enum array must report no change")
	}
	enum, ok := s["enum"].([]any)
	if !ok {
		t.Fatalf("enum must still be an array, got %T", s["enum"])
	}
	if len(enum) != 2 || enum[0] != "fast" || enum[1] != "slow" {
		t.Fatalf("enum = %v, want [fast slow] untouched", enum)
	}
}

// A legal additionalProperties SCHEMA OBJECT must survive untouched. This is
// the exact shape that panicked (`map[string]interface {}` is uncomparable).
func TestBP333LegalAdditionalPropertiesObjectIsUntouched(t *testing.T) {
	inner := map[string]any{"type": "string"}
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": inner,
	}
	if changed := CoerceSchemaKeywordTypes(s); changed {
		t.Fatal("a legal additionalProperties object must report no change")
	}
	got, ok := s["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatalf("additionalProperties must stay an object, got %T", s["additionalProperties"])
	}
	if got["type"] != "string" {
		t.Fatalf("additionalProperties = %v, want the nested schema intact", got)
	}
}

// A legal examples array, and legal scalar keywords, must all report no
// change: an already-clean schema is not rewritten (rewriting bytes the
// upstream accepts would churn the prompt cache for nothing).
func TestBP333LegalScalarsReportNoChange(t *testing.T) {
	s := map[string]any{
		"type":        "object",
		"description": "a tool",
		"minimum":     float64(1),
		"uniqueItems": true,
		"examples":    []any{map[string]any{"a": "b"}},
		"required":    []any{"a", "b"},
	}
	if changed := CoerceSchemaKeywordTypes(s); changed {
		t.Fatal("a fully legal schema must report no change")
	}
}

// The deep nesting spots must reach legal arrays/objects without panicking.
// Recursion into $defs / items / anyOf is where a legal enum array sits in
// real MCP tool definitions.
func TestBP333LegalShapesNestedDeep(t *testing.T) {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
			"opts": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "number"}},
		},
		"$defs": map[string]any{
			"Node": map[string]any{"type": "object", "enum": []any{1.0, 2.0}},
		},
		"items": map[string]any{"type": "array", "examples": []any{[]any{"x"}}},
		"anyOf": []any{
			map[string]any{"type": "string", "enum": []any{"y"}},
			map[string]any{"additionalProperties": map[string]any{"type": "boolean"}},
		},
	}
	if changed := CoerceSchemaKeywordTypes(s); changed {
		t.Fatal("legal shapes nested at every known spot must report no change")
	}
}

// Both halves at once: an illegal scalar repaired in the same node that holds
// a legal array and a legal object. The legal ones stay, the illegal one is
// fixed, and nothing panics.
func TestBP333MixedLegalAndIllegal(t *testing.T) {
	s := map[string]any{
		"enum":                 []any{"a", "b"},                        // legal, untouched
		"additionalProperties": map[string]any{"type": "string"},       // legal, untouched
		"minimum":              "1",                                    // illegal → repaired
		"uniqueItems":          "true",                                 // illegal → repaired
		"description":          123,                                    // illegal → dropped
		"required":             []any{"a", "a"},                        // dup → deduped
	}
	changed := CoerceSchemaKeywordTypes(s)
	if !changed {
		t.Fatal("the illegal keywords must be reported as changed")
	}
	if enum, ok := s["enum"].([]any); !ok || len(enum) != 2 {
		t.Fatalf("legal enum array was damaged: %v", s["enum"])
	}
	if _, ok := s["additionalProperties"].(map[string]any); !ok {
		t.Fatalf("legal additionalProperties object was damaged: %v", s["additionalProperties"])
	}
	if v, _ := s["minimum"].(float64); v != 1 {
		t.Fatalf("minimum = %v, want 1", s["minimum"])
	}
	if v, _ := s["uniqueItems"].(bool); !v {
		t.Fatalf("uniqueItems = %v, want true", s["uniqueItems"])
	}
	if _, ok := s["description"]; ok {
		t.Fatal("numeric description must be dropped")
	}
	if req, _ := s["required"].([]any); len(req) != 1 || req[0] != "a" {
		t.Fatalf("required = %v, want [a]", s["required"])
	}
}

// The function is called on every tool of every workbuddy request, so it must
// also survive shapes that are neither a schema node nor a list (a bare
// string / number / nil) rather than panicking on the type switch.
func TestBP333NonSchemaInputIsIgnored(t *testing.T) {
	for _, in := range []any{nil, "x", float64(3), true} {
		if changed := CoerceSchemaKeywordTypes(in); changed {
			t.Fatalf("%T must report no change", in)
		}
	}
}
