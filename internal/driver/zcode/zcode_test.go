package zcode

import (
	"context"
	"encoding/base64"
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

// makeJWT builds an unsigned test JWT with the given subject (signature is a
// placeholder; upstream verification is out of scope for unit tests).
func makeJWT(sub string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":%q,"iat":1700000000}`, sub)))
	return header + "." + payload + ".test-signature"
}

func testAccount() *model.Account {
	creds := map[string]any{
		"jwt":        makeJWT("user-42"),
		"device_mid": "mid-1",
	}
	raw, _ := json.Marshal(creds)
	return &model.Account{ID: "zc-1", Channel: model.ChannelZCode, CredsJSON: raw}
}

func testDriver(base string) *Driver {
	return New(config.ZCodeSettings{Enabled: true, BaseURL: base, MaxInFlight: 2}, &http.Client{})
}

func TestJWTSubExtraction(t *testing.T) {
	if got := JWTSub(makeJWT("abc-123")); got != "abc-123" {
		t.Fatalf("sub = %q", got)
	}
	if got := JWTSub("garbage"); got != "" {
		t.Fatalf("garbage should yield empty, got %q", got)
	}
}

func TestModelAliasResolution(t *testing.T) {
	cases := map[string]string{
		"glm-5.3":                 "GLM-5.3",
		"GLM-5.3-Flash":           "GLM-5.3-Flash",
		"claude-sonnet-4-6":       "GLM-5.3",
		"claude-3-5-haiku-latest": "GLM-5.3-Flash",
		"unknown-model":           "unknown-model",
	}
	for in, want := range cases {
		if got := ResolveModel(in); got != want {
			t.Fatalf("ResolveModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeBodyIdentityBlocksAndCacheControl(t *testing.T) {
	d := testDriver("http://unused")
	creds := &Creds{JWT: makeJWT("u1")}
	call := driver.ChatCall{
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "user system"}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
	raw, err := d.normalizeBody(call, creds)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["model"] != "GLM-5.3" {
		t.Fatalf("model = %v", doc["model"])
	}
	if doc["stream"] != true {
		t.Fatal("stream must be forced true")
	}
	sys := doc["system"].([]any)
	if len(sys) < 4 {
		t.Fatalf("expected official blocks + currentModel + user system, got %d blocks", len(sys))
	}
	// The dynamic currentModel block must exist.
	foundModelBlock := false
	for _, b := range sys {
		block := b.(map[string]any)
		if text, _ := block["text"].(string); strings.Contains(text, "powered by the model named GLM-5.3") {
			foundModelBlock = true
		}
	}
	if !foundModelBlock {
		t.Fatal("currentModel identity block missing")
	}
	// metadata.user_id = JWT sub.
	meta := doc["metadata"].(map[string]any)
	if meta["user_id"] != "u1" {
		t.Fatalf("metadata.user_id = %v", meta["user_id"])
	}
	// cache_control ephemeral on last block of last non-system message.
	msgs := doc["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	content := last["content"].([]any)
	lastBlock := content[len(content)-1].(map[string]any)
	if cc, ok := lastBlock["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Fatalf("cache_control missing on last block: %v", lastBlock)
	}
}

func TestAnthropicSSEParsing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != messagesPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("X-ZCode-Agent"); got != "glm" {
			t.Errorf("X-ZCode-Agent = %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: message_start`)
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`)
		fmt.Fprintln(w, `event: content_block_start`)
		fmt.Fprintln(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"的"}}`)
		fmt.Fprintln(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	var text, think string
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvText:
			text += ev.Delta
		case ir.EvThinking:
			think += ev.Delta
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text != "好的" || think != "想" {
		t.Fatalf("text=%q think=%q", text, think)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// BP-56: this upstream speaks Anthropic on the wire, so its input_tokens
// excludes cache reads/writes. ir.Usage keeps one convention — InputTokens is
// the full prompt, cache included (same as workbuddy/antigravity) — so the
// driver must fold the cache accounting into the total. Leaving it split would
// make every downstream renderer under-report the prompt (the Anthropic
// renderer subtracts the cached part again).
func TestAnthropicUsageFoldsCacheIntoTotal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: message_start`)
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":75}}}`)
		fmt.Fprintln(w, `event: message_delta`)
		fmt.Fprintln(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
		fmt.Fprintln(w, `event: message_stop`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if resp.Usage.InputTokens != 100 {
		t.Fatalf("InputTokens = %d, want 100 (20 uncached + 5 creation + 75 read)", resp.Usage.InputTokens)
	}
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 75 {
		t.Fatalf("cache = %+v", resp.Usage)
	}
	if resp.Usage.OutputTokens != 2 {
		t.Fatalf("OutputTokens = %d", resp.Usage.OutputTokens)
	}
}

func TestQuotaFamilyCodeIsModelScoped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":1310,"message":"model quota exhausted"}`)
	}))
	defer upstream.Close()
	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSoftRate {
		t.Fatalf("expected soft_rate, got %v", err)
	}
	if ue.Model != "GLM-5.3" {
		t.Fatalf("model scope = %q", ue.Model)
	}
}

func TestCode3012IsContentRejection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":3012,"message":"unusual activity"}`)
	}))
	defer upstream.Close()
	d := testDriver(upstream.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassContentBlocked {
		t.Fatalf("expected content_blocked for 3012, got %v", err)
	}
	if uperr.Rotatable(ue.Class) {
		t.Fatal("content rejection must not rotate accounts")
	}
}

func TestNewUUIDShape(t *testing.T) {
	id := newUUID()
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("uuid shape: %q", id)
	}
	_ = time.Now
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
		t.Fatalf("truncated plan body must surface as transport, got %v", err)
	}
}
