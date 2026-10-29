package prompt

import "testing"

// BP-191 (misakano 854e74cd): Kimi rejects a request whose tool schema still
// carries a "$ref" — MCP definitions share branches through "#/$defs/…". The
// expansion resolves them inline, strips the defs table, drops nothing, and
// never lets two ref sites share one expanded subtree.
func TestBP191ExpandLocalRefs(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"Item": map[string]any{"type": "string"},
		},
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/Item"},
		},
	}
	if !ExpandLocalRefs(schema) {
		t.Fatal("the expansion must report a change")
	}
	props := schema["properties"].(map[string]any)
	a := props["a"].(map[string]any)
	if a["type"] != "string" {
		t.Fatalf("the $ref was not resolved inline: %v", a)
	}
	if _, ok := schema["$defs"]; ok {
		t.Fatal("the $defs table must be stripped after expansion")
	}
}

// An unresolvable pointer stays as-is (and reports no change when there are
// no defs at all): the upstream surfaces it, same as before.
func TestBP191UnresolvableRefStays(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/Missing"}},
	}
	if ExpandLocalRefs(schema) {
		t.Fatal("no defs -> no change")
	}
	if got := schema["properties"].(map[string]any)["x"].(map[string]any)["$ref"]; got != "#/$defs/Missing" {
		t.Fatalf("an unresolvable ref must stay untouched, got %v", got)
	}
}

// Two sites referencing the same def get independent copies: editing one
// must not corrupt the other.
func TestBP191RefSitesAreIndependent(t *testing.T) {
	shared := map[string]any{
		"$defs": map[string]any{"S": map[string]any{"type": "string"}},
		"properties": map[string]any{
			"p": map[string]any{"$ref": "#/$defs/S"},
			"q": map[string]any{"$ref": "#/$defs/S"},
		},
	}
	ExpandLocalRefs(shared)
	props := shared["properties"].(map[string]any)
	p := props["p"].(map[string]any)
	q := props["q"].(map[string]any)
	if p["type"] != "string" || q["type"] != "string" {
		t.Fatalf("both sites must resolve: p=%v q=%v", p, q)
	}
	p["type"] = "number"
	if q["type"] != "string" {
		t.Fatalf("the two ref sites share one subtree: q=%v", q)
	}
}

// A defs cycle (X -> Y -> X) must terminate, not hang.
func TestBP191DefsCycleTerminates(t *testing.T) {
	schema := map[string]any{
		"$defs": map[string]any{
			"X": map[string]any{"$ref": "#/$defs/Y"},
			"Y": map[string]any{"$ref": "#/$defs/X"},
		},
		"properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/X"}},
	}
	ExpandLocalRefs(schema) // must return; depth bound stops the cycle
}
