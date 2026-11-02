package geminiv1

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-271 (CLIProxyAPI c888ff72d4, #7186): Gemini contents whose role is
// omitted or unrecognized must be inferred from their parts — a functionCall
// turn is the model speaking, a functionResponse turn is the user side — and
// otherwise alternate. The old two-way mapping stamped every non-"model"
// turn "user", so an unlabeled functionCall turn reached the IR as a user
// message carrying tool calls.
func mustParse(t *testing.T, body string) *ir.ChatRequest {
	t.Helper()
	req, err := ParseRequest([]byte(body), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return req
}

func TestBP271OmittedRoleWithFunctionCallIsAssistant(t *testing.T) {
	// The middle unlabeled text turn already alternates to assistant, so the
	// third turn's parts inference is the ONLY thing that can still call the
	// functionCall turn assistant — alternation alone would flip it to user.
	req := mustParse(t, `{"contents":[
		{"role":"user","parts":[{"text":"weather?"}]},
		{"parts":[{"text":"Let me check."}]},
		{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Tokyo"}}}]}
	]}`)
	var call *ir.Message
	for i := range req.Messages {
		m := &req.Messages[i]
		if len(m.ToolCalls) > 0 {
			call = m
		}
	}
	if call == nil {
		t.Fatalf("no tool-call message in %+v", req.Messages)
	}
	if call.Role != ir.RoleAssistant {
		t.Fatalf("unlabeled functionCall turn role = %q, want assistant (parts inference)", call.Role)
	}
}

func TestBP271UnlabeledPlainTextTurnsAlternate(t *testing.T) {
	req := mustParse(t, `{"contents":[
		{"parts":[{"text":"hi"}]},
		{"parts":[{"text":"hello again"}]}
	]}`)
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != ir.RoleUser {
		t.Fatalf("first unlabeled turn = %q, want user", req.Messages[0].Role)
	}
	if req.Messages[1].Role != ir.RoleAssistant {
		t.Fatalf("second unlabeled turn = %q, want assistant (alternation)", req.Messages[1].Role)
	}
}

func TestBP271ExplicitRolesUntouched(t *testing.T) {
	req := mustParse(t, `{"contents":[
		{"role":"user","parts":[{"text":"q"}]},
		{"role":"model","parts":[{"text":"a"}]}
	]}`)
	if req.Messages[0].Role != ir.RoleUser || req.Messages[1].Role != ir.RoleAssistant {
		t.Fatalf("explicit roles drifted: %q %q", req.Messages[0].Role, req.Messages[1].Role)
	}
}

// The function-role regression guard: a tool-result turn keeps extracting as
// a RoleTool message regardless of its label, and a functionResponse-only
// turn must not leave an empty user shell behind.
func TestBP271FunctionRoleStillYieldsToolResult(t *testing.T) {
	req := mustParse(t, `{"contents":[
		{"role":"user","parts":[{"text":"weather?"}]},
		{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Tokyo"}}}]},
		{"role":"function","parts":[{"functionResponse":{"name":"get_weather","response":{"result":"Sunny"}}}]}
	]}`)
	var tool *ir.Message
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role == ir.RoleTool {
			tool = m
		}
	}
	if tool == nil || tool.ToolName != "get_weather" {
		t.Fatalf("tool result missing: %+v", req.Messages)
	}
	for _, m := range req.Messages {
		if m.Role == ir.RoleUser && len(m.Content) == 0 && len(m.ToolCalls) == 0 {
			t.Fatalf("empty user shell leaked into the IR: %+v", req.Messages)
		}
	}
}

// snake_case part keys: some clients emit function_call / function_response
// (CLIProxyAPI added both spellings; encoding/json's case-folding does not
// cover the underscore).
func TestBP271SnakeCaseFunctionParts(t *testing.T) {
	var w GenerateRequest
	body := `{"contents":[{"role":"model","parts":[
		{"function_call":{"name":"ping","args":{"x":1}}},
		{"function_response":{"name":"ping","response":{"result":"pong"}}}
	]}]}`
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(w.Contents) != 1 || len(w.Contents[0].Parts) != 2 {
		t.Fatalf("contents parsed wrong: %+v", w.Contents)
	}
	if w.Contents[0].Parts[0].FunctionCall == nil || w.Contents[0].Parts[0].FunctionCall.Name != "ping" {
		t.Fatalf("snake_case function_call lost: %+v", w.Contents[0].Parts[0])
	}
	if w.Contents[0].Parts[1].FunctionResp == nil || w.Contents[0].Parts[1].FunctionResp.Name != "ping" {
		t.Fatalf("snake_case function_response lost: %+v", w.Contents[0].Parts[1])
	}

	// And the wire path keeps them: the unlabeled-response turn still pairs.
	req := mustParse(t, `{"contents":[
		{"role":"user","parts":[{"text":"ping?"}]},
		{"parts":[{"function_call":{"name":"ping","args":{"x":1}}}]},
		{"parts":[{"function_response":{"name":"ping","response":{"result":"pong"}}}]}
	]}`)
	var calls, results int
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 {
			calls++
		}
		if m.Role == ir.RoleTool {
			results++
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("snake_case round trip: calls=%d results=%d in %+v", calls, results, req.Messages)
	}
}
