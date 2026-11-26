// BP-356 (CLIProxyAPI aa1389a3 / c8419674): a NAMED tool_choice must become
// allowedFunctionNames. The old code read only req.ToolChoice.(string), so
// "required" became mode:ANY with no name — the model was free to call ANY
// tool even when the client asked for one specific function.
//
// aistudio has always written it (BP-326); antigravity never did.
package antigravity

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

func tooledReq(choice any, name string) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:          "gemini-4-hydrogen",
		ToolChoice:     choice,
		ToolChoiceName: name,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "weather?"}}},
		},
		Tools: []ir.ToolDef{{Name: "get_weather"}, {Name: "get_time"}},
	}
}

// The Anthropic ingress shape: {"type":"tool","name":"get_weather"}.
func TestBP356MapToolChoiceWritesAllowedName(t *testing.T) {
	req := tooledReq(map[string]any{"type": "tool", "name": "get_weather"}, "")
	fc := functionCallingConfigOf(t, req)
	if fc["mode"] != "ANY" {
		t.Fatalf("mode = %v, want ANY", fc["mode"])
	}
	names, _ := fc["allowedFunctionNames"].([]any)
	if len(names) != 1 || names[0] != "get_weather" {
		t.Fatalf("allowedFunctionNames = %v — without it the model may call any tool", names)
	}
}

// The separately-parsed name (Anthropic ingress fills ToolChoiceName without a
// ToolChoice string). It must still pin the tool.
func TestBP356BareNamePinsTheTool(t *testing.T) {
	req := tooledReq(nil, "get_time")
	fc := functionCallingConfigOf(t, req)
	if fc["mode"] != "ANY" {
		t.Fatalf("mode = %v, want ANY (a named tool means 'call this one')", fc["mode"])
	}
	names, _ := fc["allowedFunctionNames"].([]any)
	if len(names) != 1 || names[0] != "get_time" {
		t.Fatalf("allowedFunctionNames = %v", names)
	}
}

// OpenAI's {"type":"function","function":{"name":"x"}}.
func TestBP356OpenAIFunctionShape(t *testing.T) {
	req := tooledReq(map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}, "")
	fc := functionCallingConfigOf(t, req)
	names, _ := fc["allowedFunctionNames"].([]any)
	if len(names) != 1 || names[0] != "get_weather" {
		t.Fatalf("allowedFunctionNames = %v", names)
	}
}

// "required" with NO name keeps mode ANY and writes no allow-list — the client
// genuinely left the choice open.
func TestBP356RequiredWithoutNameAllowsAny(t *testing.T) {
	fc := functionCallingConfigOf(t, tooledReq("required", ""))
	if fc["mode"] != "ANY" {
		t.Fatalf("mode = %v", fc["mode"])
	}
	if _, present := fc["allowedFunctionNames"]; present {
		t.Fatalf("an empty allow-list would block every tool: %v", fc["allowedFunctionNames"])
	}
}

// The modes that must NOT change.
func TestBP356NoneAndAuto(t *testing.T) {
	if fc := functionCallingConfigOf(t, tooledReq("none", "")); fc["mode"] != "NONE" {
		t.Fatalf("none → %v", fc["mode"])
	}
	if fc := functionCallingConfigOf(t, tooledReq("auto", "")); fc["mode"] != "AUTO" {
		t.Fatalf("auto → %v", fc["mode"])
	}
	// An unnamed "none" with a stray name must NOT be upgraded to ANY.
	if fc := functionCallingConfigOf(t, tooledReq("none", "get_weather")); fc["mode"] != "NONE" {
		t.Fatalf("none with a stray name → %v, want NONE", fc["mode"])
	}
}

// Tool arguments that are not an object must ride as {"params": …} instead of
// being silently emptied (CLIProxyAPI c975104f/f7137853).
func TestBP356NonObjectArgsWrappedInHistory(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-4-hydrogen",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "go"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
				{ID: "c1", Name: "Bash", Arguments: `["ls","-l"]`},
			}},
			{Role: ir.RoleTool, ToolCallID: "c1", ToolName: "Bash", Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
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
	if !containsMarker(doc, "params") {
		t.Fatalf("an array argument body was dropped instead of wrapped: %s", body)
	}
}

// helpers ---------------------------------------------------------------------

func functionCallingConfigOf(t *testing.T, req *ir.ChatRequest) map[string]any {
	t.Helper()
	body, err := buildRequest("proj", req.Model, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	inner := findInner(t, doc)
	tc, ok := inner["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("no toolConfig on the wire: %v", inner)
	}
	fcc, ok := tc["functionCallingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("no functionCallingConfig: %v", tc)
	}
	return fcc
}

// containsMarker walks the whole document looking for a key.
func containsMarker(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[key]; ok {
			return true
		}
		for _, vv := range t {
			if containsMarker(vv, key) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if containsMarker(vv, key) {
				return true
			}
		}
	}
	return false
}
