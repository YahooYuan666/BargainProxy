// BP-358 (CLIProxyAPI 531aea15 / 38067a29): two wire-shape defects in the
// antigravity request builder.
//
//  1. A schema that travelled through a protobuf encoder carries "type" as the
//     enum's NUMBER (google.api.Type: 1=STRING … 6=OBJECT). The old code
//     returned nil for those, and the caller DROPS a nil value — the whole
//     "type" key vanished and the declaration lost its type.
//  2. systemInstruction went out as role-less {parts:[…]}; the upstream expects
//     the same shape a content turn has (an explicit role, one part).
package antigravity

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

// geminiTypeField is the one place the numeric form is normalised.
func TestBP358NumericSchemaType(t *testing.T) {
	cases := map[float64]string{
		1: "STRING",
		2: "NUMBER",
		3: "INTEGER",
		4: "BOOLEAN",
		5: "ARRAY",
		6: "OBJECT",
	}
	for n, want := range cases {
		got, _ := geminiTypeField(n)
		if got != want {
			t.Fatalf("type %v → %v, want %q", n, got, want)
		}
	}
	// Outside the enum is not a type: the caller's drop-the-key behaviour stays.
	for _, n := range []float64{0, 7, 6.5, -1} {
		if got, _ := geminiTypeField(n); got != nil {
			t.Fatalf("type %v → %v, want nil (not an enum value)", n, got)
		}
	}
	// JSON gives us float64; an int from a Go-built schema must work too.
	if got, _ := geminiTypeField(int(6)); got != "OBJECT" {
		t.Fatalf("int(6) → %v", got)
	}
}

// The string and array forms are untouched by this change.
func TestBP358StringAndArrayFormsUnchanged(t *testing.T) {
	if got, _ := geminiTypeField("object"); got != "OBJECT" {
		t.Fatalf("string form → %v", got)
	}
	got, nullable := geminiTypeField([]any{"string", "null"})
	if got != "STRING" || !nullable {
		t.Fatalf("array form → %v nullable=%v", got, nullable)
	}
	// A protobuf-shaped array (numeric + null) must also resolve.
	got, nullable = geminiTypeField([]any{float64(1), "null"})
	if got != "STRING" || !nullable {
		t.Fatalf("numeric array form → %v nullable=%v", got, nullable)
	}
}

// The whole request path: a numeric type survives cleanGeminiSchema and lands
// on the wire as the name. This is the assertion a helper-level test misses.
func TestBP358NumericTypeSurvivesBuild(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-4-hydrogen",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		Tools: []ir.ToolDef{{
			Name: "Bash",
			Parameters: map[string]any{
				"type": float64(6), // google.api.Type TYPE_OBJECT
				"properties": map[string]any{
					"command": map[string]any{"type": float64(1)}, // TYPE_STRING
				},
			},
		}},
	}
	body, err := buildRequest("proj", "gemini-4-hydrogen", req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	fd := findDecl(t, doc, "Bash")
	params, _ := fd["parameters"].(map[string]any)
	if params["type"] != "OBJECT" {
		t.Fatalf(`declaration type = %v, want "OBJECT" (a numeric protobuf type must not be dropped)`, params["type"])
	}
	props, _ := params["properties"].(map[string]any)
	cmd, _ := props["command"].(map[string]any)
	if cmd["type"] != "STRING" {
		t.Fatalf(`nested type = %v, want "STRING"`, cmd["type"])
	}
}

// systemInstruction must carry role:"user" and exactly ONE part.
func TestBP358SystemInstructionRoleAndSinglePart(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-4-hydrogen",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{
				{Kind: "text", Text: "first block"},
				{Kind: "text", Text: "second block"},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
	}
	body, err := buildRequest("proj", "gemini-4-hydrogen", req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	inner := findInner(t, doc)
	si, ok := inner["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("no systemInstruction in %v", inner)
	}
	if si["role"] != "user" {
		t.Fatalf(`systemInstruction.role = %v, want "user"`, si["role"])
	}
	parts, _ := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("systemInstruction has %d parts, want exactly 1 (the upstream expects the content-turn shape)", len(parts))
	}
	p0, _ := parts[0].(map[string]any)
	text, _ := p0["text"].(string)
	if text != "first block\n\nsecond block" {
		t.Fatalf("joined text = %q — both blocks must survive", text)
	}
}

// helpers ---------------------------------------------------------------------

// findDecl digs the named declaration out of request.tools[].functionDeclarations.
func findDecl(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	inner := findInner(t, doc)
	tools, _ := inner["tools"].([]any)
	for _, tl := range tools {
		tm, _ := tl.(map[string]any)
		decls, _ := tm["functionDeclarations"].([]any)
		for _, dl := range decls {
			dm, _ := dl.(map[string]any)
			if dm["name"] == name {
				return dm
			}
		}
	}
	t.Fatalf("declaration %q not found in %v", name, inner["tools"])
	return nil
}

// findInner returns request{...} — the envelope this driver builds.
func findInner(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	inner, ok := doc["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request envelope in %v", doc)
	}
	return inner
}
