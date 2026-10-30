package opencodezen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// envSkipLive skips a live (network) check unless explicitly enabled.
func envSkipLive(t *testing.T) bool {
	if os.Getenv("BARGAINPROXY_LIVE_ZEN") == "" {
		t.Skip("live network check; set BARGAINPROXY_LIVE_ZEN=1 to run")
	}
	return false
}

// The free lane serves most models on /v1/chat/completions, but some only on
// /v1/responses (models.dev declares their SDK as @ai-sdk/openai). The catalog
// decides the lane per model; the driver must route accordingly and refuse
// models that have no lane at all (jev's systemone).
func TestStreamRoutesPerLane(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == "/responses" {
			w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
			w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
			return
		}
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	d := New(config.OpenCodeZenSettings{BaseURL: srv.URL}, srv.Client())
	d.SetWireResolver(func(id string) string {
		switch {
		case strings.HasPrefix(id, "muse-spark"):
			return wireResponses
		case strings.HasPrefix(id, "jev"):
			return ""
		}
		return wireChat
	})
	emit := func(ir.StreamEvent) error { return nil }
	call := driver.ChatCall{UpstreamModel: "beam-free", Request: &ir.ChatRequest{
		Model: "beam-free", Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}}

	call.UpstreamModel = "muse-spark-1.3-contributor-free"
	if _, err := d.Stream(context.Background(), call, emit); err != nil {
		t.Fatalf("responses lane: %v", err)
	}
	call.UpstreamModel = "beam-free"
	if _, err := d.Stream(context.Background(), call, emit); err != nil {
		t.Fatalf("chat lane: %v", err)
	}
	call.UpstreamModel = "jev-1.13-free"
	if _, err := d.Stream(context.Background(), call, emit); err == nil {
		t.Fatal("a model with no supported lane must be refused")
	}
	want := "/responses,/chat/completions"
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("upstream paths = %q, want %q", got, want)
	}
}

// The responses lane demands the flat tool schema and must never receive
// tool_choice:"none" (upstream 400) — both traps are pinned here.
func TestBuildResponsesWireShape(t *testing.T) {
	d := New(config.OpenCodeZenSettings{}, nil)
	call := driver.ChatCall{UpstreamModel: "muse-spark-1.3-contributor-free", Request: &ir.ChatRequest{
		Model: "muse-spark-1.3-contributor-free", Stream: true, MaxTokens: 64, ToolChoice: "none",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "be brief"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "weather?"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"sf"}`}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "get_weather",
				Content: []ir.ContentPart{{Kind: "text", Text: "sunny"}}},
		},
		Tools: []ir.ToolDef{{Name: "get_weather", Description: "w", Parameters: map[string]any{"type": "object"}}},
	}}
	raw, err := d.buildResponsesWire(call)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// BP-127: instructions end with OpenCode's environment block.
	if ins, _ := body["instructions"].(string); !strings.HasPrefix(ins, "be brief") || !zenHasEnvironment.MatchString(ins) {
		t.Fatalf("instructions must keep the prompt and carry the env block: %q", body["instructions"])
	}
	if body["stream"] != true {
		t.Fatal("streaming is a hard free-lane requirement")
	}
	if body["max_output_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens missing: %v", body["max_output_tokens"])
	}
	if _, ok := body["tool_choice"]; ok {
		t.Fatal(`tool_choice:"none" must never reach the responses lane`)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1+len(gateToolNames) {
		t.Fatalf("tools = %d, want 1 real + %d gate", len(tools), len(gateToolNames))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		tm := tl.(map[string]any)
		if _, nested := tm["function"]; nested {
			t.Fatal("responses-lane tools must be flat (a nested function object is a 400)")
		}
		names[tm["name"].(string)] = true
	}
	if !names["get_weather"] || !names["shell"] || !names["read"] {
		t.Fatalf("tools missing: %v", names)
	}
	input, _ := body["input"].([]any)
	// The empty assistant turn contributes no message item — only its call.
	if len(input) != 3 {
		t.Fatalf("input items = %d, want user + function_call + function_call_output", len(input))
	}
	if input[0].(map[string]any)["type"] != "message" {
		t.Fatalf("user item shape: %v", input[0])
	}
	if input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("tool items shape: %v / %v", input[1], input[2])
	}
}

func TestParseResponsesSSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"think"}`,
		`data: {"type":"response.output_text.delta","delta":"po"}`,
		`data: {"type":"response.output_text.delta","delta":"ng"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"shell"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"cmd\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"ls\"}"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":7}}}}`,
		``,
	}, "\n\n") + "\n\n"

	var evs []ir.StreamEvent
	resp, err := parseResponsesSSE(context.Background(), strings.NewReader(stream), func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := resp.Message.Content[0].Text; got != "pong" {
		t.Fatalf("text = %q", got)
	}
	if resp.Thinking != "think" {
		t.Fatalf("thinking = %q", resp.Thinking)
	}
	// The gate tool must be stripped from the surfaced tool calls.
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("gate tool leaked into the response: %+v", resp.Message.ToolCalls)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 3 || !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.StopReason != ir.StopEnd {
		t.Fatalf("stop = %v", resp.StopReason)
	}
	_ = evs
}

// Real tool calls arriving as deltas survive; the terminal object is only a
// fallback for calls that never streamed.
func TestParseResponsesSSEToolCallAndFallback(t *testing.T) {
	streamed := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":\"sf\"}"}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"sf\"}"}]}}`,
	}, "\n\n") + "\n\n"
	resp, err := parseResponsesSSE(context.Background(), strings.NewReader(streamed), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("want exactly one tool call (no duplication from the fallback), got %+v", resp.Message.ToolCalls)
	}
	if tc := resp.Message.ToolCalls[0]; tc.Name != "get_weather" || tc.Arguments != `{"city":"sf"}` || tc.ID != "call_1" {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Fatalf("stop = %v, want tool_use", resp.StopReason)
	}

	// No deltas at all: the completed object must still yield the call.
	// ("read" would be stripped as a gate tool — use a real tool name here.)
	onlyCompleted := `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{}"}]}}` + "\n\n"
	resp2, err := parseResponsesSSE(context.Background(), strings.NewReader(onlyCompleted), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp2.Message.ToolCalls) != 1 || resp2.Message.ToolCalls[0].ID != "call_2" {
		t.Fatalf("fallback tool call = %+v", resp2.Message.ToolCalls)
	}
}

// A tool round-trip must carry tool_calls on the tool-only assistant turn and
// tool_call_id on the tool result. Folding those fields into the content
// branch dropped them, and upstream rejects such a history as an invalid
// request — the 400 every tool round-trip used to hit.
func TestBuildChatWireToolRoundTrip(t *testing.T) {
	d := New(config.OpenCodeZenSettings{}, nil)
	call := driver.ChatCall{UpstreamModel: "space-bunny-free", Request: &ir.ChatRequest{
		Model: "space-bunny-free", Stream: true,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "weather?"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"sf"}`}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "get_weather",
				Content: []ir.ContentPart{{Kind: "text", Text: "18C"}}},
		},
	}}
	raw, err := d.buildWire(call)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// BP-127: the request carried no system message, so the OpenCode env
	// block rides in as one first.
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (3 turns + injected env system)", len(body.Messages))
	}
	if env, _ := body.Messages[0]["content"].(string); !zenHasEnvironment.MatchString(env) {
		t.Fatalf("first message must be the env system block, got %v", body.Messages[0])
	}
	asst := body.Messages[2]
	if _, hasContent := asst["content"]; hasContent {
		t.Fatalf("tool-only assistant turn must omit content: %v", asst)
	}
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls missing: %v", asst)
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"sf"}` {
		t.Fatalf("tool call mangled: %v", fn)
	}
	toolMsg := body.Messages[3]
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "18C" {
		t.Fatalf("tool result shape wrong: %v", toolMsg)
	}
}

// Each ingress speaks its own tool_choice dialect; forwarding the raw value
// upstream is an invalid request (Anthropic "any"/"tool", OpenAI's function
// object). The mapping is per lane — and "none" must never reach responses.
func TestToolChoiceNormalization(t *testing.T) {
	named := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
	flatNamed := map[string]any{"type": "function", "name": "get_weather"}
	cases := []struct {
		name          string
		in            any
		wantChat      any
		wantResponses any
	}{
		{"auto", "auto", "auto", "auto"},
		{"required", "required", "required", "required"},
		{"none", "none", "none", nil},
		{"anthropic-any", "any", "required", "required"},
		{"anthropic-any-map", map[string]any{"type": "any"}, "required", "required"},
		{"anthropic-tool", map[string]any{"type": "tool", "name": "get_weather"}, named, flatNamed},
		{"openai-function", map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}, named, flatNamed},
		{"responses-flat", map[string]any{"type": "function", "name": "get_weather"}, named, flatNamed},
		{"unknown", "banana", nil, nil},
	}
	for _, c := range cases {
		if got := chatToolChoice(c.in, ""); !reflect.DeepEqual(got, c.wantChat) {
			t.Errorf("%s: chat = %#v, want %#v", c.name, got, c.wantChat)
		}
		if got := responsesToolChoice(c.in, ""); !reflect.DeepEqual(got, c.wantResponses) {
			t.Errorf("%s: responses = %#v, want %#v", c.name, got, c.wantResponses)
		}
	}
}

// The upstream rejects any request with more than 128 tools. A real ZCode turn
// with a few MCP servers exceeds that, and a hard failure kills the whole turn
// — so the driver caps deterministically (client order) and keeps going.
func TestToolCapKeepsTheTurnAlive(t *testing.T) {
	d := New(config.OpenCodeZenSettings{}, nil)
	defs := []ir.ToolDef{{Name: "get_weather", Parameters: map[string]any{"type": "object"}}}
	for i := 0; i < 200; i++ {
		defs = append(defs, ir.ToolDef{Name: fmt.Sprintf("t%03d", i)})
	}
	call := driver.ChatCall{UpstreamModel: "space-bunny-free", Request: &ir.ChatRequest{
		Model: "space-bunny-free", Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		Tools:    defs,
	}}
	builds := map[string]func(driver.ChatCall) ([]byte, error){
		"chat":      d.buildWire,
		"responses": d.buildResponsesWire,
	}
	for lane, build := range builds {
		raw, err := build(call)
		if err != nil {
			t.Fatalf("%s: build: %v", lane, err)
		}
		var body struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: unmarshal: %v", lane, err)
		}
		if len(body.Tools) != maxUpstreamTools {
			t.Fatalf("%s: tools = %d, want exactly %d (client + %d gate stubs)", lane, len(body.Tools), maxUpstreamTools, len(gateToolNames))
		}
		if got := toolNameOf(body.Tools[0]); got != "get_weather" {
			t.Fatalf("%s: first client tool lost: %q", lane, got)
		}
		if names := toolNameOf(body.Tools[len(body.Tools)-1]); names != "read" && names != "bash" {
			t.Fatalf("%s: gate tools missing at the tail: %q", lane, names)
		}
	}
}

// Images used to be dropped silently on both lanes.
func TestImageContentRenders(t *testing.T) {
	d := New(config.OpenCodeZenSettings{}, nil)
	call := driver.ChatCall{UpstreamModel: "space-bunny-free", Request: &ir.ChatRequest{
		Model: "space-bunny-free", Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: "what is this?"},
			{Kind: "image", ImageMIME: "image/png", ImageB64: "aGk="},
		}}},
	}}
	raw, err := d.buildWire(call)
	if err != nil {
		t.Fatalf("chat build: %v", err)
	}
	if !strings.Contains(string(raw), `"image_url":{"url":"data:image/png;base64,aGk="}`) {
		t.Fatalf("chat wire must carry the image as a data URL: %s", raw)
	}
	raw, err = d.buildResponsesWire(call)
	if err != nil {
		t.Fatalf("responses build: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"input_image"`) ||
		!strings.Contains(string(raw), `"image_url":"data:image/png;base64,aGk="`) {
		t.Fatalf("responses wire must carry the image: %s", raw)
	}
}

// Opt-in live check: the model that motivated this lane must actually answer
// through it (BARGAINPROXY_LIVE_ZEN=1).
func TestLiveMuseResponsesLane(t *testing.T) {
	if envSkipLive(t) {
		return
	}
	d := New(config.OpenCodeZenSettings{}, nil)
	d.SetWireResolver(func(id string) string {
		if strings.HasPrefix(id, "muse-spark") {
			return wireResponses
		}
		return wireChat
	})
	var text strings.Builder
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		UpstreamModel: "muse-spark-1.3-contributor-free",
		Request: &ir.ChatRequest{Model: "muse-spark-1.3-contributor-free", Stream: true, MaxTokens: 512,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "只回答两个字：收到"}}}}},
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("live responses lane: %v", err)
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Fatalf("live responses lane returned no text: %+v", resp)
	}
	t.Logf("live muse reply: %q (usage %+v)", text.String(), resp.Usage)
}
