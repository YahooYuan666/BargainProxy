package aistudio

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func bp326Account(t *testing.T, key string) *model.Account {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"api_key": key})
	if err != nil {
		t.Fatal(err)
	}
	return &model.Account{ID: "aistudio:test", Channel: model.ChannelAIStudio, CredsJSON: raw}
}

// TestBP326StreamShapeAndEvents pins the wire contract: URL path + query, key
// header, payload mapping (system→systemInstruction, tool→functionResponse,
// tool_choice, schema sanitizer, thinking level) and the event stream
// (thought / text / functionCall / usage / done with StopToolUse).
func TestBP326StreamShapeAndEvents(t *testing.T) {
	var gotPath, gotKey, gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("x-goog-api-key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"plan\",\"thought\":true}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"SF\"}},\"thoughtSignature\":\"SIG\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":11,\"candidatesTokenCount\":5,\"thoughtsTokenCount\":3,\"cachedContentTokenCount\":4}}\n\n")
	}))
	defer srv.Close()

	d := New(config.AIStudioSettings{BaseURL: srv.URL}, nil)
	req := &ir.ChatRequest{
		Model:     "gemini-4-hydrogen",
		MaxTokens: 77,
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "be nice"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_0_ab", Name: "get_weather", Arguments: `{"city":"SF"}`}}},
			{Role: ir.RoleTool, ToolCallID: "call_0_ab", ToolName: "get_weather",
				Content: []ir.ContentPart{{Kind: "text", Text: `{"temp":"20"}`}}},
		},
		Tools: []ir.ToolDef{{
			Name: "get_weather", Description: "weather",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "anyOf": []any{map[string]any{"type": "string"}}},
				},
				"required": []any{"city"},
				"$schema":  "http://json-schema.org/draft-07/schema#",
			},
		}},
		Thinking: &ir.ThinkingConfig{Enabled: true, Level: "high"},
	}
	var events []ir.StreamEvent
	out, err := d.Stream(context.Background(), driver.ChatCall{
		Account: bp326Account(t, "K-test"), UpstreamModel: "gemini-4-hydrogen", Request: req,
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if gotPath != "/models/gemini-4-hydrogen:streamGenerateContent" || gotQuery != "alt=sse" {
		t.Fatalf("wire: path=%q query=%q", gotPath, gotQuery)
	}
	if gotKey != "K-test" {
		t.Fatalf("api key header = %q", gotKey)
	}

	// Payload mapping.
	if _, ok := gotBody["systemInstruction"]; !ok {
		t.Fatalf("systemInstruction missing from payload: %v", gotBody)
	}
	contents, _ := gotBody["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d (want 3: user, model+toolcall, tool result)", len(contents))
	}
	first := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("contents[0].role = %v", first["role"])
	}
	assistant := contents[1].(map[string]any)
	if assistant["role"] != "model" {
		t.Fatalf("contents[1].role = %v (assistant history maps to model)", assistant["role"])
	}
	toolTurn := contents[2].(map[string]any)
	toolParts, _ := toolTurn["parts"].([]any)
	fnResp, _ := toolParts[0].(map[string]any)["functionResponse"].(map[string]any)
	if fnResp == nil || fnResp["name"] != "get_weather" {
		t.Fatalf("functionResponse = %v", toolParts[0])
	}
	respObj, _ := fnResp["response"].(map[string]any)
	if respObj["temp"] != "20" {
		t.Fatalf("functionResponse.response = %v (want the parsed result object)", respObj)
	}
	tools, _ := gotBody["tools"].([]any)
	decls, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	decl, _ := decls[0].(map[string]any)
	if decl["name"] != "get_weather" || decl["description"] != "weather" {
		t.Fatalf("declaration = %v", decl)
	}
	params, _ := decl["parameters"].(map[string]any)
	if _, ok := params["$schema"]; ok {
		t.Fatalf("schema sanitizer kept $schema: %v", params)
	}
	city, _ := params["properties"].(map[string]any)["city"].(map[string]any)
	if _, ok := city["anyOf"]; ok {
		t.Fatalf("schema sanitizer kept anyOf: %v", city)
	}
	if city["type"] != "string" {
		t.Fatalf("schema sanitizer lost type: %v", city)
	}
	genCfg, _ := gotBody["generationConfig"].(map[string]any)
	if genCfg["maxOutputTokens"] != float64(77) {
		t.Fatalf("maxOutputTokens = %v", genCfg["maxOutputTokens"])
	}
	th, _ := genCfg["thinkingConfig"].(map[string]any)
	if th["thinkingLevel"] != "HIGH" || th["includeThoughts"] != true {
		t.Fatalf("thinkingConfig = %v", th)
	}

	// Event stream.
	wantKinds := []ir.StreamEventKind{
		ir.EvThinking, ir.EvText, ir.EvText,
		ir.EvToolStart, ir.EvToolArgs,
		ir.EvText, ir.EvUsage, ir.EvDone,
	}
	if len(events) != len(wantKinds) {
		for i, ev := range events {
			t.Logf("event[%d] = %s %q", i, ev.Kind, ev.Delta)
		}
		t.Fatalf("got %d events, want %d", len(events), len(wantKinds))
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Fatalf("event[%d] = %s, want %s", i, events[i].Kind, k)
		}
	}
	if events[0].Delta != "plan" {
		t.Fatalf("thinking delta = %q", events[0].Delta)
	}
	if events[1].Delta != "Hel" || events[2].Delta != "lo" || events[5].Delta != "!" {
		t.Fatalf("text deltas = %q %q %q", events[1].Delta, events[2].Delta, events[5].Delta)
	}
	if tc := events[3].ToolCall; tc == nil || tc.Name != "get_weather" || tc.ID == "" {
		t.Fatalf("tool start = %+v", events[3].ToolCall)
	}
	if !strings.Contains(events[4].Delta, "SF") {
		t.Fatalf("tool args delta = %q", events[4].Delta)
	}
	if u := events[6].Usage; u == nil || u.InputTokens != 11 || u.OutputTokens != 5 || u.ReasoningTokens != 3 || u.CachedInputTokens != 4 || !u.CacheKnown {
		t.Fatalf("usage = %+v", events[6].Usage)
	}
	if events[7].Stop != ir.StopToolUse {
		t.Fatalf("done stop = %v (function calls were emitted)", events[7].Stop)
	}

	// Aggregated response.
	if out.StopReason != ir.StopToolUse || out.Thinking != "plan" {
		t.Fatalf("out stop/thinking = %v/%q", out.StopReason, out.Thinking)
	}
	if txt := out.Message.Content[0].Text; txt != "Hello!" {
		t.Fatalf("content text = %q", txt)
	}
	if len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("tool calls = %+v", out.Message.ToolCalls)
	}
}

// TestBP326ToolResultError pins BP-224's vocabulary on this channel: a failed
// tool result rides functionResponse.response.error, not the success payload.
func TestBP326ToolResultError(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer srv.Close()

	d := New(config.AIStudioSettings{BaseURL: srv.URL}, nil)
	req := &ir.ChatRequest{
		Model: "gemini-4-hydrogen",
		Messages: []ir.Message{
			{Role: ir.RoleTool, ToolCallID: "call_x", ToolName: "bash", IsError: true,
				Content: []ir.ContentPart{{Kind: "text", Text: "exit 1: permission denied"}}},
		},
	}
	if _, err := d.Stream(context.Background(), driver.ChatCall{Account: bp326Account(t, "K"), UpstreamModel: "gemini-4-hydrogen", Request: req},
		func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	contents, _ := gotBody["contents"].([]any)
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	fnResp, _ := parts[0].(map[string]any)["functionResponse"].(map[string]any)
	if fnResp == nil || fnResp["name"] != "bash" {
		t.Fatalf("functionResponse = %v", parts[0])
	}
	respObj, _ := fnResp["response"].(map[string]any)
	if respObj["error"] != "exit 1: permission denied" {
		t.Fatalf("response.error = %v", respObj)
	}
}

// TestBP326ErrorClassification pins the non-200 taxonomy for the official API.
func TestBP326ErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass string
		wantModel bool
		wantHard  bool
		wantReset bool // expect ResetAt ≈ now+30s
	}{
		{"400 invalid key", 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`, uperr.ClassAuth, false, false, false},
		{"400 bad request", 400, `{"error":{"code":400,"message":"Unknown name \"foo\" at 'tools[0]'","status":"INVALID_ARGUMENT"}}`, uperr.ClassBadRequest, false, false, false},
		{"429 with retry", 429, `{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`, uperr.ClassModelRate, true, true, true},
		{"429 bare", 429, `{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED"}}`, uperr.ClassUpstreamBusy, true, false, false},
		{"503 high demand", 503, `{"error":{"code":503,"message":"The model is overloaded","status":"UNAVAILABLE"}}`, uperr.ClassUpstreamBusy, false, false, false},
		{"404 model", 404, `{"error":{"code":404,"message":"Model not found","status":"NOT_FOUND"}}`, uperr.ClassNotFound, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			d := New(config.AIStudioSettings{BaseURL: srv.URL}, nil)
			_, err := d.Stream(context.Background(), driver.ChatCall{
				Account: bp326Account(t, "K"), UpstreamModel: "gemini-4-hydrogen",
				Request: &ir.ChatRequest{Model: "gemini-4-hydrogen", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
			}, func(ir.StreamEvent) error { return nil })
			ue, ok := uperr.As(err)
			if !ok {
				t.Fatalf("want uperr, got %v", err)
			}
			if ue.Class != tc.wantClass {
				t.Fatalf("class = %s (%s), want %s", ue.Class, ue.Message, tc.wantClass)
			}
			if tc.wantModel != (ue.Model == "gemini-4-hydrogen") {
				t.Fatalf("model scoping = %q", ue.Model)
			}
			if tc.wantHard != ue.ResetHard {
				t.Fatalf("reset hard = %v", ue.ResetHard)
			}
			if tc.wantReset {
				if ue.ResetAt.IsZero() {
					t.Fatalf("ResetAt zero")
				}
				d := time.Until(ue.ResetAt)
				if d < 25*time.Second || d > 35*time.Second {
					t.Fatalf("ResetAt = %v, want ~30s", d)
				}
			}
		})
	}
}

// TestBP326MidStreamErrorFrame: Gemini can open a 200 SSE stream and then
// report an error frame — ignoring it would book the request as a success
// (antigravity ticket-36 family).
func TestBP326MidStreamErrorFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"code\":503,\"message\":\"The model is overloaded\",\"status\":\"UNAVAILABLE\"}}\n\n")
	}))
	defer srv.Close()
	d := New(config.AIStudioSettings{BaseURL: srv.URL}, nil)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account: bp326Account(t, "K"), UpstreamModel: "gemini-4-hydrogen",
		Request: &ir.ChatRequest{Model: "gemini-4-hydrogen",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("want upstream_busy, got %v", err)
	}
}

// TestBP326Probe: the account probe is a ListModels call — 200 means the key
// is alive; errors classify so the scheduler can rest the account honestly.
func TestBP326Probe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("x-goog-api-key") == "" {
			t.Errorf("probe wire: path=%q key=%q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-4-hydrogen"},{"name":"models/gemini-4-argon"}]}`)
	}))
	defer srv.Close()
	d := New(config.AIStudioSettings{BaseURL: srv.URL}, nil)
	pr, err := d.Probe(context.Background(), bp326Account(t, "K"))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(pr.Note, "2") {
		t.Fatalf("note = %q (want the model count)", pr.Note)
	}
}
