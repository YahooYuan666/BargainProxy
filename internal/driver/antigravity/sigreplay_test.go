// BP-44: real thought_signature capture + replay (experimental toggle).
package antigravity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// --- cache unit behaviour ---

func TestThoughtSigCacheBasics(t *testing.T) {
	c := newThoughtSigCache()
	c.put("acc1", "m1", "call_0_abcd", "get_weather", "SIG-REAL")
	if sig, ok := c.get("acc1", "m1", "call_0_abcd", "get_weather"); !ok || sig != "SIG-REAL" {
		t.Fatalf("get = %q/%v, want SIG-REAL/true", sig, ok)
	}
	// 名称不匹配 → 未命中（客户端复用 ID 不得串味）
	if _, ok := c.get("acc1", "m1", "call_0_abcd", "delete_file"); ok {
		t.Fatal("name mismatch must miss")
	}
	// 换模型 → 未命中
	if _, ok := c.get("acc1", "m2", "call_0_abcd", "get_weather"); ok {
		t.Fatal("model mismatch must miss")
	}
	// 空值不写
	c.put("acc1", "m1", "call_1_x", "n", "")
	if _, ok := c.get("acc1", "m1", "call_1_x", "n"); ok {
		t.Fatal("empty signature must not be stored")
	}
}

func TestThoughtSigCacheExpiryAndCap(t *testing.T) {
	c := newThoughtSigCache()
	// 过期：直接塞一条旧时间戳
	c.mu.Lock()
	c.entries[thoughtSigKey{account: "a", model: "m", callID: "old"}] = thoughtSigEntry{name: "n", sig: "S", at: time.Now().Add(-3 * thoughtSigTTL)}
	c.mu.Unlock()
	if _, ok := c.get("a", "m", "old", "n"); ok {
		t.Fatal("expired entry must miss and be dropped")
	}
	// 上限：塞满后插入仍不超上限
	for i := 0; i < thoughtSigMax+10; i++ {
		c.put("a", "m", fmt.Sprintf("c%d", i), "n", "S")
	}
	if n := c.size(); n > thoughtSigMax {
		t.Fatalf("size = %d, want <= %d", n, thoughtSigMax)
	}
}

// --- capture → replay round trip (stream mock + body assertion) ---

const realSig = "REAL-SIG-abcdefghijklmnopqrstuvwxyz-0123456789"

func toolStreamDriver(t *testing.T, sig string) (*Driver, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"functionCall":{"name":"get_weather","args":{"city":"BJ"}},"thoughtSignature":"`+sig+`"}`, `{"promptTokenCount":5,"candidatesTokenCount":3}`, "STOP"))
	}))
	t.Cleanup(upstream.Close)
	return testDriver(upstream.URL, upstream.URL+"/token"), upstream
}

func streamToolCall(t *testing.T, d *Driver) *ir.ChatResponse {
	t.Helper()
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(t),
		UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{
			Model:    "gemini-3.8-flash-high",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "查天气"}}}},
		},
	}, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", resp.Message.ToolCalls)
	}
	return resp
}

func replayBody(t *testing.T, d *Driver, tc ir.ToolCall) string {
	t.Helper()
	req := &ir.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "查天气"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{tc}},
			{Role: ir.RoleTool, ToolCallID: tc.ID, ToolName: tc.Name, Content: []ir.ContentPart{{Kind: "text", Text: `{"temp":18}`}}},
		},
	}
	lookup, _ := d.sigLookup("gemini-3.8-flash-high", "ag-1")
	raw, err := buildRequest("proj", "gemini-3.8-flash-high", req, lookup)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	return string(raw)
}

func TestRealSignatureCapturedAndReplayedWhenEnabled(t *testing.T) {
	d, _ := toolStreamDriver(t, realSig)
	d.SetThoughtSignatureReplay(true)

	resp := streamToolCall(t, d)
	tc := resp.Message.ToolCalls[0]

	body := replayBody(t, d, tc)
	if !strings.Contains(body, realSig) {
		t.Fatalf("replayed body must carry the REAL signature, got: %s", body)
	}
	if strings.Contains(body, skipThoughtSignature) {
		t.Fatalf("replayed body must not fall back to the placeholder when the cache hit: %s", body)
	}
}

func TestReplayFallsBackToPlaceholderWhenDisabledOrMiss(t *testing.T) {
	// 开关关闭：不捕获、不回填
	d, _ := toolStreamDriver(t, realSig)
	resp := streamToolCall(t, d)
	tc := resp.Message.ToolCalls[0]
	body := replayBody(t, d, tc)
	if strings.Contains(body, realSig) {
		t.Fatalf("disabled toggle must not attach a real signature: %s", body)
	}
	if !strings.Contains(body, skipThoughtSignature) {
		t.Fatalf("disabled toggle must keep the placeholder path: %s", body)
	}

	// 开关打开但缓存未命中（例如网关重启后的旧会话）→ 占位符，绝不空手
	d2, _ := toolStreamDriver(t, realSig)
	d2.SetThoughtSignatureReplay(true)
	unknown := ir.ToolCall{ID: "call_9_nope", Name: "get_weather", Arguments: `{"city":"BJ"}`}
	body2 := replayBody(t, d2, unknown)
	if !strings.Contains(body2, skipThoughtSignature) {
		t.Fatalf("cache miss must fall back to the placeholder: %s", body2)
	}
}

// Signatures are bound to the upstream account (sub2api field evidence,
// gemini_v1beta_handler.go 2026-10): replaying one captured under another
// account must MISS instead of sending a doomed signature.
func TestSignatureDoesNotCrossAccounts(t *testing.T) {
	d, _ := toolStreamDriver(t, realSig) // captured under testAccount ("ag-1")
	d.SetThoughtSignatureReplay(true)
	resp := streamToolCall(t, d)
	tc := resp.Message.ToolCalls[0]

	req := &ir.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "查天气"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{tc}},
			{Role: ir.RoleTool, ToolCallID: tc.ID, ToolName: tc.Name, Content: []ir.ContentPart{{Kind: "text", Text: `{"temp":18}`}}},
		},
	}
	lookup, used := d.sigLookup("gemini-3.8-flash-high", "ag-2") // different account
	raw, err := buildRequest("proj", "gemini-3.8-flash-high", req, lookup)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if *used {
		t.Fatal("cross-account lookup must not count as a hit")
	}
	if strings.Contains(string(raw), realSig) {
		t.Fatal("cross-account replay must not attach another account's signature")
	}
	if !strings.Contains(string(raw), skipThoughtSignature) {
		t.Fatalf("cross-account replay must fall back to the placeholder: %s", raw)
	}
}

// --- stale-signature self-heal ---

// The experiment must never be worse than the placeholder path: if the
// upstream rejects a replayed signature (account/model switch, expiry), the
// driver retries once WITHOUT it.
func TestStaleSignatureSelfHealsWithPlaceholder(t *testing.T) {
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts. This is required for tools to work correctly."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"text":"healed"}`, "", "STOP"))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	d.SetThoughtSignatureReplay(true)
	stale := ir.ToolCall{ID: "call_7_stale", Name: "get_weather", Arguments: `{"city":"BJ"}`}
	d.sigCache.put("ag-1", "gemini-3.8-flash-high", stale.ID, stale.Name, "STALE-SIGNATURE-VALUE")

	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(t),
		UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{
			Model: "gemini-3.8-flash-high",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "查天气"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{stale}},
				{Role: ir.RoleTool, ToolCallID: stale.ID, ToolName: stale.Name, Content: []ir.ContentPart{{Kind: "text", Text: `{"temp":18}`}}},
			},
		},
	}, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream must self-heal, got: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream requests = %d, want 2 (retry with placeholder)", len(bodies))
	}
	if !strings.Contains(bodies[0], "STALE-SIGNATURE-VALUE") {
		t.Fatal("first attempt must carry the cached (stale) signature")
	}
	if !strings.Contains(bodies[1], skipThoughtSignature) {
		t.Fatal("retry must fall back to the placeholder")
	}
	if got := resp.Message.Content[0].Text; got != "healed" {
		t.Fatalf("content = %q, want healed", got)
	}
}

// Without a replayed signature the rejection is surfaced as-is (no retry).
func TestSignatureRejectionWithoutReplayIsNotRetried(t *testing.T) {
	reqCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts."}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	d.SetThoughtSignatureReplay(true) // on, but the cache has nothing

	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(t),
		UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{
			Model:    "gemini-3.8-flash-high",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("want the upstream rejection surfaced")
	}
	if reqCount != 1 {
		t.Fatalf("requests = %d, want 1 (no retry without a replayed signature)", reqCount)
	}
}
