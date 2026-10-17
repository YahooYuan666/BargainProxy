package workbuddy

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
	"bargainproxy/internal/prompt"
	"bargainproxy/internal/uperr"
)

func fakeToken(kind string) string {
	return strings.Join([]string{"test", kind, "value"}, "-")
}

func testAccount() *model.Account {
	creds := map[string]any{
		"access_token": fakeToken("access"),
		"uid":          "u-100",
		"realm":        "cn",
		"expires_at":   time.Now().Add(48 * time.Hour).Unix(),
	}
	raw, _ := json.Marshal(creds)
	return &model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: raw}
}

func testDriver(chatBase string) *Driver {
	return New(config.WorkBuddySettings{
		Enabled:    true,
		ChatBaseCN: chatBase,
		ChatBaseGL: chatBase,
		PromptMode: "custom",
	}, &http.Client{})
}

func TestStreamBasicAndForcedStream(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"你"},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_content":"想…"},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	acc := testAccount()

	var gotThink, gotText string
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       acc,
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			// Client asked for non-stream; upstream must still get stream:true.
			Stream: false,
		},
	}, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvText:
			gotText += ev.Delta
		case ir.EvThinking:
			gotThink += ev.Delta
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if gotBody["stream"] != true {
		t.Fatalf("upstream must receive stream:true, got %v", gotBody["stream"])
	}
	if gotText != "你好" || gotThink != "想…" {
		t.Fatalf("text=%q think=%q", gotText, gotThink)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	// System prompt replacement: a gateway system message must be present.
	msgs := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("expected injected system prompt, got %v", first["role"])
	}
}

func TestSystemReplacementDropsClientSystem(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "CLIENT SECRET SYSTEM PROMPT"}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected system+user, got %d messages", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if content, _ := sys["content"].(string); strings.Contains(content, "CLIENT SECRET") {
		t.Fatal("client system prompt leaked through")
	}
}

func TestEmptyNameToolCallsPrunedFromOutboundBody(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "run tools"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
					{ID: "call_bad", Name: "", Arguments: "{}"}, // empty name: dropped
					{ID: "call_ok", Name: "lookup", Arguments: `{"q":"x"}`},
				}},
				{Role: ir.RoleTool, ToolCallID: "call_bad", Content: []ir.ContentPart{{Kind: "text", Text: "orphaned result"}}},
				{Role: ir.RoleTool, ToolCallID: "call_ok", ToolName: "lookup", Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
			},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs := gotBody["messages"].([]any)
	var assistant []any
	var toolIDs []any
	for _, mi := range msgs {
		m := mi.(map[string]any)
		if m["role"] == "assistant" {
			assistant = m["tool_calls"].([]any)
		}
		if m["role"] == "tool" {
			toolIDs = append(toolIDs, m["tool_call_id"])
		}
	}
	if len(assistant) != 1 {
		t.Fatalf("outbound tool_calls = %v, want only the named one", assistant)
	}
	tc := assistant[0].(map[string]any)
	if tc["id"] != "call_ok" {
		t.Fatalf("kept wrong tool call: %v", tc)
	}
	if len(toolIDs) != 1 || toolIDs[0] != "call_ok" {
		t.Fatalf("orphaned tool result must be pruned symmetrically, got %v", toolIDs)
	}
}

// TestPrunedAssistantShellDropped: an assistant message that held ONLY an
// empty-name tool call (no text) would serialize as {"role":"assistant"}
// after the 11133 pruning — a shape strictly-validated upstreams reject with
// 400. The empty shell must be dropped; a sibling assistant message with
// real content must survive.
func TestPrunedAssistantShellDropped(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "go"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
					{ID: "call_bad", Name: "", Arguments: "{}"}, // sole tool call, empty name
				}}, // no Content: becomes an empty shell after pruning
				{Role: ir.RoleTool, ToolCallID: "call_bad", Content: []ir.ContentPart{{Kind: "text", Text: "orphan"}}},
				{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "real reply"}}},
			},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs := gotBody["messages"].([]any)
	for _, mi := range msgs {
		m := mi.(map[string]any)
		if m["role"] != "assistant" {
			continue
		}
		_, hasContent := m["content"]
		_, hasToolCalls := m["tool_calls"]
		if !hasContent && !hasToolCalls {
			t.Fatalf("empty assistant shell must be dropped, outbound messages: %v", msgs)
		}
	}
	// The content-bearing assistant message survives.
	found := false
	for _, mi := range msgs {
		m := mi.(map[string]any)
		if m["role"] == "assistant" && m["content"] == "real reply" {
			found = true
		}
	}
	if !found {
		t.Fatalf("content-bearing assistant message lost: %v", msgs)
	}
}

func TestSanitizeStripsFingerprintKeys(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[],"cc_version":"1.2.3","x-anthropic-billing-hdr":"x","metadata":{"anthropic_beta":"1","keep":"yes"}}`)
	out := sanitizeCheck(t, raw)
	if _, ok := out["cc_version"]; ok {
		t.Fatal("cc_version not stripped")
	}
	if _, ok := out["x-anthropic-billing-hdr"]; ok {
		t.Fatal("x-anthropic-billing-hdr not stripped")
	}
	md := out["metadata"].(map[string]any)
	if _, ok := md["anthropic_beta"]; ok {
		t.Fatal("nested anthropic_beta not stripped")
	}
	if md["keep"] != "yes" {
		t.Fatal("innocent keys must survive sanitization")
	}
}

func sanitizeCheck(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	body := prompt.SanitizeBody(raw)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal sanitized: %v", err)
	}
	return out
}

func TestModelScoped6004(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"code":6004,"message":"模型调用频率超限，将在 2026-10-14 16:30:00 重置"}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassModelRate {
		t.Fatalf("expected model_rate, got %v", err)
	}
	if ue.Model != "glm-5.3" {
		t.Fatalf("model scope = %q", ue.Model)
	}
	want := time.Date(2026, 10, 14, 16, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if !ue.ResetAt.Equal(want) {
		t.Fatalf("reset = %s, want %s", ue.ResetAt, want)
	}
}

// TestQueueFull6021IsUpstreamBusy: the CodeBuddy inference-queue-full event
// (HTTP 429 + business code 6021 "queue.full.title") is an upstream capacity
// problem, NOT an account problem. It must classify as upstream_busy (no
// rotation, no account penalty) and carry the model scope for the
// channel-level pause.
func TestQueueFull6021IsUpstreamBusy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":6021,"msg":"queue.full.title","requestId":"req-1"}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "deepseek-v4.1-flash",
		Request:       &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("expected upstream_busy for 6021, got %v", err)
	}
	if ue.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model scope = %q, want deepseek-v4.1-flash", ue.Model)
	}
	// Not rotatable: switching accounts against the same full queue is the
	// miskill failure mode.
	if uperr.Rotatable(ue.Class) {
		t.Fatal("upstream_busy must not rotate accounts")
	}
}

func TestHTTP402IsHardCredit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		fmt.Fprint(w, `{"message":"积分不足"}`)
	}))
	defer upstream.Close()
	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, _ := uperr.As(err)
	if ue == nil || ue.Class != uperr.ClassHardCredit {
		t.Fatalf("expected hard_credit, got %v", err)
	}
}

func TestToolCallsAggregated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_time","arguments":"{\"tz\":"}}]},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "time?"}}}},
			Tools:    []ir.ToolDef{{Name: "get_time"}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "get_time" || tc.Arguments != `{"tz":"UTC"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Fatalf("stop = %s", resp.StopReason)
	}
}

func TestProbeParsesCreditsWithExpiry(t *testing.T) {
	soon := time.Now().Add(3 * 24 * time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
	later := time.Now().Add(60 * 24 * time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
	resp := fmt.Sprintf(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
	  {"PackageName":"A","CapacityRemain":100,"CapacitySize":100,"CycleEndTime":%q},
	  {"PackageName":"B","CapacityRemain":250,"CapacitySize":250,"CycleEndTime":%q},
	  {"PackageName":"C","CapacityRemain":0,"CapacitySize":50,"CycleEndTime":%q}
	]}}}}`, soon, later, later)

	credits, err := parseCredits([]byte(resp), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if credits.Remain != 350 || credits.Total != 400 {
		t.Fatalf("remain/total = %d/%d, want 350/400", credits.Remain, credits.Total)
	}
	if credits.Expiring != 100 {
		t.Fatalf("expiring = %d, want 100 (only pack A within 7d)", credits.Expiring)
	}
	if credits.EarliestRemaining != 100 {
		t.Fatalf("earliest remaining = %d, want 100", credits.EarliestRemaining)
	}
	if credits.EarliestExpiresAt.IsZero() || !credits.EarliestExpiresAt.After(time.Now()) {
		t.Fatalf("earliest expiry = %s", credits.EarliestExpiresAt)
	}
}

func TestLegacyAuthImport(t *testing.T) {
	legacy := fmt.Sprintf(`{
	  "account":{"uid":"u1","nickname":"小明","enterpriseId":"e1"},
	  "auth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"realm":"cn"}
	}`, fakeToken("access"), fakeToken("refresh"), time.Now().Add(time.Hour).Unix())
	creds, err := ParseLegacyAuth([]byte(legacy))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if creds.UID != "u1" || creds.RealmOrDefault() != "cn" || creds.AccessToken == "" {
		t.Fatalf("creds = %+v", creds)
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

func TestProbeReadTruncatedBodyFails(t *testing.T) {
	d := testDriver(truncatedBodyServer(t).URL)
	_, err := d.Probe(context.Background(), testAccount())
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("truncated billing body must surface as transport, got %v", err)
	}
}

// TestPromptReplaceHotSwitch: the panel switch turns system-prompt
// replacement off at runtime without touching config; off degrades to the
// passthrough behaviour (system messages stripped).
func TestPromptReplaceHotSwitch(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	call := driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "client system prompt"}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
	run := func(d *Driver) []any {
		t.Helper()
		if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error { return nil }); err != nil {
			t.Fatalf("stream: %v", err)
		}
		return gotBody["messages"].([]any)
	}

	d := testDriver(upstream.URL) // prompt_mode custom, replacement on
	msgs := run(d)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] == "client system prompt" {
		t.Fatalf("replacement on: first message = %v", first)
	}

	d.SetPromptReplaceEnabled(false)
	msgs = run(d)
	for _, m := range msgs {
		if m.(map[string]any)["role"] == "system" {
			t.Fatalf("replacement off must strip system messages, got %v", msgs)
		}
	}
}
