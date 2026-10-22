package trae

import (
	"context"
	"encoding/json"
	"fmt"
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

func fakeToken(kind string) string {
	return strings.Join([]string{"test", kind, "value"}, "-")
}

func testAccount() *model.Account {
	creds := map[string]any{
		"access_token": fakeToken("jwt"),
		"uid":          "u-7",
		"device_id":    "dev-1",
		"machine_id":   "mac-1",
	}
	raw, _ := json.Marshal(creds)
	return &model.Account{ID: "trae-1", Channel: model.ChannelTrae, CredsJSON: raw}
}

func testDriver(agentBase, ugBase string) *Driver {
	return New(config.TraeSettings{Enabled: true, AgentHost: agentBase, UgHost: ugBase, MaxInFlight: 1}, &http.Client{})
}

func TestSOLOBodyShape(t *testing.T) {
	call := driver.ChatCall{
		UpstreamModel: "glm-5.2",
		Request: &ir.ChatRequest{
			Model: "glm-5.2",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "t1", Name: "f", Arguments: `{"x":1}`}}},
				{Role: ir.RoleTool, ToolCallID: "t1", ToolName: "f", Content: []ir.ContentPart{{Kind: "text", Text: "42"}}},
			},
			Tools: []ir.ToolDef{{Name: "f"}},
		},
	}
	raw, err := buildSOLOBody(call, functionSOLO)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["function"] != "solo_work_lite" {
		t.Fatalf("function = %v", doc["function"])
	}
	if doc["stream"] != true {
		t.Fatalf("stream must be forced true, got %v", doc["stream"])
	}
	if doc["model"] != "glm-5.2" {
		t.Fatalf("model = %v", doc["model"])
	}
	if _, ok := doc["config_name"]; ok {
		t.Fatal("config_name must NOT be injected (breaks newer models)")
	}
	msgs := doc["messages"].([]any)
	// First message content must be block array form.
	first := msgs[0].(map[string]any)
	content := first["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content not block-array: %v", content)
	}
	// Assistant tool call must be function_call shape.
	assistant := msgs[1].(map[string]any)
	tcs := assistant["tool_calls"].([]any)
	fc := tcs[0].(map[string]any)
	if _, ok := fc["function_call"]; !ok {
		t.Fatalf("tool_call not converted to function_call: %v", fc)
	}
}

// BP-48: the upstream FunctionDefinition types tools[].function.parameters as
// a JSON *string* — an object body dies with code=4001 ("cannot unmarshal
// object into Go struct field … of type string", live 2026-10-16). Nameless
// tools are dropped: one bad entry rejects the whole request.
func TestSOLOToolParametersAreJSONStrings(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
		"required":   []any{"city"},
	}
	call := driver.ChatCall{
		UpstreamModel: "glm-5.3-flashx",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3-flashx",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Tools: []ir.ToolDef{
				{Name: "get_weather", Description: "w", Parameters: schema},
				{Name: "no_schema"},
				{Name: "  "}, // nameless → dropped
			},
		},
	}
	raw, err := buildSOLOBody(call, functionSOLO)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Decoding into a string-typed field is the exact upstream shape check:
	// an object here fails unmarshal instead of silently passing.
	var typed struct {
		Tools []struct {
			Function struct {
				Name       string `json:"name"`
				Parameters string `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatalf("parameters must be a JSON string for the upstream struct: %v (%s)", err, raw)
	}
	if len(typed.Tools) != 2 {
		t.Fatalf("nameless tool must be dropped: %s", raw)
	}
	if typed.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tool order/name changed: %s", raw)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(typed.Tools[0].Function.Parameters), &back); err != nil {
		t.Fatalf("parameters string must be the JSON schema: %q", typed.Tools[0].Function.Parameters)
	}
	if back["type"] != "object" {
		t.Fatalf("schema mangled: %q", typed.Tools[0].Function.Parameters)
	}
	// A tool without a schema omits the key entirely (no empty-string schema).
	var loose struct {
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(raw, &loose)
	fn := loose.Tools[1]["function"].(map[string]any)
	if _, present := fn["parameters"]; present {
		t.Fatalf("schema-less tool must omit parameters: %v", fn)
	}
}

func TestStreamParsesChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/v3/llm_utils_chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("X-App-Id"); got != appID {
			t.Errorf("X-App-Id = %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Cloud-IDE-JWT ") {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"你"},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL)
	var text string
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.2",
		Request:       &ir.ChatRequest{Model: "glm-5.2", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text += ev.Delta
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text != "你好" {
		t.Fatalf("text = %q", text)
	}
	if resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestPlanLimit1005(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"code":1005,"message":"plan entitlement exhausted"}`)
	}))
	defer upstream.Close()
	d := testDriver(upstream.URL, upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.2",
		Request:       &ir.ChatRequest{Model: "glm-5.2", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSoftRate {
		t.Fatalf("expected soft_rate for 1005, got %v", err)
	}
	if delay := time.Until(ue.ResetAt); delay < 11*time.Hour {
		t.Fatalf("1005 cooldown should be ~12h, got %s", delay)
	}
}

func TestCheckinFlow(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/trae/api/v2/ug/checkin_credits/status":
			fmt.Fprint(w, `{"checked_in":false,"credits":100,"enable":true}`)
		case "/trae/api/v2/ug/checkin_credits/claim":
			fmt.Fprint(w, `{"code":0}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	checkedIn, credits, _, err := d.Checkin(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !checkedIn || credits != 100 || calls != 2 {
		t.Fatalf("checkedIn=%v credits=%d calls=%d", checkedIn, credits, calls)
	}
}

// BP-98 TRAE-3: a business refusal is upstream's answer about today — 9074
// ("check in via the app") answers the same however often it is asked, so it
// fails on the FIRST claim; the retry loop is for 5xx/transport only
// (yetone/magpie#808: three 20s waits per account per slot bought nothing).
func TestCheckinClaimBusinessCodeRetriesThenFails(t *testing.T) {
	old := checkinRetryDelay
	checkinRetryDelay = time.Millisecond
	defer func() { checkinRetryDelay = old }()
	claims := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trae/api/v2/ug/checkin_credits/status":
			fmt.Fprint(w, `{"checked_in":false,"credits":100,"enable":true}`)
		case "/trae/api/v2/ug/checkin_credits/claim":
			claims++
			fmt.Fprint(w, `{"code":9074,"message":"当前参与用户太多"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	checked, _, _, err := d.Checkin(context.Background(), testAccount())
	if err == nil {
		t.Fatal("business code 9074 must fail the checkin")
	}
	if checked {
		t.Fatal("checkedIn must be false when the claim is rejected")
	}
	if claims != 1 {
		t.Fatalf("claim attempts = %d, want 1 (a refusal is not retryable)", claims)
	}
	if !strings.Contains(err.Error(), "9074") {
		t.Fatalf("error should name the business code: %v", err)
	}
}

func TestEntUsageAggregation(t *testing.T) {
	raw := []byte(`{"user_entitlement_pack_list":[
	  {"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":250.7}},
	  {"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}
	]}`)
	remain, limit, used, packs, err := ParseEntUsage(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if limit != 1500 || used != 350 || remain != 1150 || packs != 2 {
		t.Fatalf("remain=%d limit=%d used=%d packs=%d", remain, limit, used, packs)
	}
}

// truncatedBodyServer declares 100 bytes of JSON but delivers 12, so the
// client-side io.ReadAll fails with an unexpected EOF mid-body.
func truncatedBodyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack unsupported")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"partial\":")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeUsageReadTruncatedBodyFails(t *testing.T) {
	d := testDriver("", truncatedBodyServer(t).URL)
	_, err := d.Probe(context.Background(), testAccount())
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("truncated usage body must surface as transport, got %v", err)
	}
}

func TestCheckinStatusReadTruncatedBodyFails(t *testing.T) {
	d := testDriver("", truncatedBodyServer(t).URL)
	_, _, _, err := d.Checkin(context.Background(), testAccount())
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("truncated checkin status body must surface as transport, got %v", err)
	}
}

// BP-48: the live wire is a named-event SSE (event:output / token_usage / done
// with blank-line framing). The old chunks-only parser silently produced empty
// completions — this test pins text, thinking, streamed tool-call fragments,
// usage and the finish reason.
func TestStreamParsesSOLONamedEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			"id:1",
			"event:metadata",
			`data:{"model":"","session_id":"s-1"}`,
			"",
			"id:2",
			"event:timing_cost",
			`data:{"name":"llm_raw_chat_v2","queue_timing":22}`,
			"",
			"event:output",
			`data:{"response":"","reasoning_content":"用户想","tool_calls":null}`,
			"",
			"event:output",
			`data:{"response":"你好","reasoning_content":"","tool_calls":null}`,
			"",
			"event:output",
			`data:{"response":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function_call":{"name":"get_weather","arguments":"{\"ci"}}]}`,
			"",
			"event:output",
			`data:{"response":"","tool_calls":[{"index":0,"function_call":{"arguments":"ty\":\"BJ\"}"}}]}`,
			"",
			"event:extra_info",
			`data:{"reasoning_content":"ignored full version"}`,
			"",
			"event:token_usage",
			`data:{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}`,
			"",
			"event:done",
			`data:{"finish_reason":"tool_calls"}`,
			"",
		}, "\n"))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL)
	var text, thinking, args string
	var toolStarts int
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3-flashx",
		Request:       &ir.ChatRequest{Model: "glm-5.3-flashx", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvText:
			text += ev.Delta
		case ir.EvThinking:
			thinking += ev.Delta
		case ir.EvToolStart:
			toolStarts++
		case ir.EvToolArgs:
			args += ev.Delta
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text != "你好" {
		t.Fatalf("text = %q (named events must be parsed)", text)
	}
	if thinking != "用户想" {
		t.Fatalf("thinking = %q", thinking)
	}
	if toolStarts != 1 {
		t.Fatalf("toolStarts = %d", toolStarts)
	}
	if args != `{"city":"BJ"}` {
		t.Fatalf("streamed args = %q", args)
	}
	if len(resp.Message.ToolCalls) != 1 ||
		resp.Message.ToolCalls[0].Name != "get_weather" ||
		resp.Message.ToolCalls[0].Arguments != `{"city":"BJ"}` ||
		resp.Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Fatalf("stop = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage.InputTokens != 21 || resp.Usage.OutputTokens != 142 || resp.Usage.ReasoningTokens != 135 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// A mid-stream event:error frame must classify as a business error (1005 →
// plan-limit soft cooldown), not pass through as an empty success.
func TestStreamSOLOErrorEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata:{\"response\":\"部分\",\"tool_calls\":null}\n\nevent:error\ndata:{\"code\":1005,\"message\":\"plan entitlement exhausted\"}\n\n")
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3-flashx",
		Request:       &ir.ChatRequest{Model: "glm-5.3-flashx", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSoftRate {
		t.Fatalf("expected soft_rate from event:error, got %v", err)
	}
}

// BP-48: a payload-shape rejection is the request's fault, not the account's —
// it must classify as bad_request (400) so the client sees the real reason
// instead of a masked 500 internal.
func TestBadRequest4001Classified(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":4001,"message":"bad request: json: cannot unmarshal object into Go struct field FunctionDefinition.tools.function.parameters of type string"}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3-flashx",
		Request:       &ir.ChatRequest{Model: "glm-5.3-flashx", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassBadRequest {
		t.Fatalf("expected bad_request for 4001, got %v", err)
	}
	if ue.Status != 400 {
		t.Fatalf("status = %d, want 400", ue.Status)
	}
}
