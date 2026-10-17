package antigravity

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

// fakeToken assembles a placeholder token at runtime so no credential-shaped
// literal appears in the source tree.
func fakeToken(kind string) string {
	return strings.Join([]string{"test", kind, "value"}, "-")
}

func testAccount(t *testing.T) *model.Account {
	t.Helper()
	creds := map[string]any{
		"access_token":  fakeToken("access"),
		"refresh_token": fakeToken("refresh"),
		"expiry":        time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		"project_id":    "test-project",
	}
	raw, _ := json.Marshal(creds)
	return &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity, CredsJSON: raw}
}

func testDriver(baseURL, tokenURL string) *Driver {
	cfg := config.AntigravitySettings{
		Enabled:                 true,
		BaseURL:                 baseURL,
		TokenURL:                tokenURL,
		MaxInFlight:             3,
		DailyURL:                baseURL, // no fallback in tests
		GoogleOAuthClientID:     fakeToken("client-id"),
		GoogleOAuthClientSecret: fakeToken("client-secret"),
	}
	return New(cfg, &http.Client{})
}

// sseFrame builds an antigravity-style SSE response frame.
func sseFrame(partsJSON string, usage string, finish string) string {
	usagePart := ""
	if usage != "" {
		usagePart = `"usageMetadata":` + usage + `,`
	}
	return fmt.Sprintf(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[%s]},"finishReason":%q}],%s"modelVersion":"test"}}`,
		partsJSON, finish, usagePart)
}

func TestStreamEmitsTextAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fakeToken("access") {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"text":"Hello "}`, "", "STOP"))
		fmt.Fprintln(w, sseFrame(`{"text":"world"}`, `{"promptTokenCount":10,"candidatesTokenCount":5}`, ""))
		fmt.Fprintln(w, sseFrame(`{"text":"!"}`, "", ""))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	acc := testAccount(t)

	var events []ir.StreamEvent
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       acc,
		UpstreamModel: "gemini-pro-agent",
		Request: &ir.ChatRequest{
			Model:    "gemini-3-pro",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text string
	for _, ev := range events {
		if ev.Kind == ir.EvText {
			text += ev.Delta
		}
	}
	if text != "Hello world!" {
		t.Fatalf("text = %q", text)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.StopReason != ir.StopEnd {
		t.Fatalf("stop = %s", resp.StopReason)
	}
}

func TestStreamToolCallMapping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"functionCall":{"name":"get_weather","args":{"city":"SH"}}}`, "", "STOP"))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	acc := testAccount(t)

	var toolName, toolArgs string
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       acc,
		UpstreamModel: "gemini-pro-agent",
		Request:       &ir.ChatRequest{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "weather?"}}}}},
	}, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvToolStart:
			toolName = ev.ToolCall.Name
		case ir.EvToolArgs:
			toolArgs = ev.Delta
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if toolName != "get_weather" {
		t.Fatalf("tool name = %q", toolName)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolArgs), &parsed); err != nil || parsed["city"] != "SH" {
		t.Fatalf("tool args = %q", toolArgs)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Fatalf("stop = %s", resp.StopReason)
	}
}

func TestQuotaFailureResetParsing(t *testing.T) {
	body := `{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generate_content_free_tier_requests"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"537s"}]}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, body)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	acc := testAccount(t)

	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       acc,
		UpstreamModel: "gemini-pro-agent",
		Request:       &ir.ChatRequest{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSoftRate {
		t.Fatalf("expected soft_rate, got %v", err)
	}
	if ue.ResetAt.IsZero() {
		t.Fatal("expected parsed reset time")
	}
	delay := time.Until(ue.ResetAt)
	if delay < 500*time.Second || delay > 600*time.Second {
		t.Fatalf("reset delay ~537s expected, got %s", delay)
	}
}

func TestParseQuotaWindows(t *testing.T) {
	reset5h := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	resetWk := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{
	  "groups":[{"buckets":[
	    {"bucketId":"gemini-3-pro-5h","remainingFraction":0.42,"resetTime":%q},
	    {"bucketId":"gemini-3-pro-weekly","remainingFraction":0.87,"resetTime":%q}
	  ]}]}`, reset5h, resetWk)

	windows, err := ParseQuotaWindows([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if windows["five_hour"].RemainingFraction != 0.42 {
		t.Fatalf("5h = %+v", windows["five_hour"])
	}
	if windows["weekly"].RemainingFraction != 0.87 {
		t.Fatalf("weekly = %+v", windows["weekly"])
	}
}

func TestBuildRequestShape(t *testing.T) {
	temp := 0.7
	req := &ir.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "be brief"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
		},
		Temperature: &temp,
		MaxTokens:   100,
		Tools:       []ir.ToolDef{{Name: "f", Description: "d", Parameters: map[string]any{"type": "object"}}},
	}
	raw, err := buildRequest("proj-1", "gemini-pro-agent", req, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["project"] != "proj-1" || doc["model"] != "gemini-pro-agent" || doc["requestType"] != "agent" || doc["userAgent"] != "antigravity" {
		t.Fatalf("outer shape wrong: %v", doc)
	}
	inner := doc["request"].(map[string]any)
	if _, ok := inner["systemInstruction"]; !ok {
		t.Fatal("systemInstruction missing")
	}
	if _, ok := inner["tools"]; !ok {
		t.Fatal("tools missing")
	}
	gen := inner["generationConfig"].(map[string]any)
	if gen["temperature"].(float64) != 0.7 || gen["maxOutputTokens"].(float64) != 100 {
		t.Fatalf("generationConfig = %v", gen)
	}
}

// TestBuildRequestCleansToolSchema: the exact live-observed 400 shape —
// OpenAI-style parameters carrying $schema / additionalProperties and nested
// "schema" keys — must come out of buildRequest reduced to the Gemini subset.
func TestBuildRequestCleansToolSchema(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		Tools: []ir.ToolDef{{
			Name: "get_time", Description: "get current time",
			Parameters: map[string]any{
				"$schema":              "http://json-schema.org/draft-07/schema#",
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"tz": map[string]any{"type": "string", "description": "timezone"},
				},
				"required": []any{"tz"},
			},
		}},
	}
	raw, err := buildRequest("proj-1", "gemini-pro-agent", req, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(raw)
	for _, banned := range []string{"$schema", "additionalProperties"} {
		if len(s) >= len(banned) && (func() bool {
			for i := 0; i+len(banned) <= len(s); i++ {
				if s[i:i+len(banned)] == banned {
					return true
				}
			}
			return false
		})() {
			t.Fatalf("banned key %q reached the wire: %s", banned, s)
		}
	}
}

func TestRefreshTokenFlow(t *testing.T) {
	var gotGrant string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, fakeToken("rotated"))
	}))
	defer tokenSrv.Close()

	d := testDriver("http://unused", tokenSrv.URL)
	// Expired token forces refresh.
	creds := map[string]any{
		"access_token":  fakeToken("stale"),
		"refresh_token": fakeToken("refresh"),
		"expiry":        time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	raw, _ := json.Marshal(creds)
	acc := &model.Account{ID: "ag-1", Channel: model.ChannelAntigravity, CredsJSON: raw}
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotGrant != "refresh_token" {
		t.Fatalf("grant = %q", gotGrant)
	}
	newCreds, _ := ParseCreds(acc.CredsJSON)
	if newCreds.AccessToken != fakeToken("rotated") {
		t.Fatalf("token = %q", newCreds.AccessToken)
	}
	if !acc.CredsDirty {
		t.Fatal("creds should be marked dirty for persistence")
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

func TestQuotaProbeReadTruncatedBodyFails(t *testing.T) {
	d := testDriver(truncatedBodyServer(t).URL, "")
	if _, err := d.fetchQuota(context.Background(), "ag-1", fakeToken("access")); err == nil {
		t.Fatal("truncated quota body must surface as an error, not an empty window map")
	}
}

func TestLoadCodeAssistReadTruncatedBodyFails(t *testing.T) {
	d := testDriver(truncatedBodyServer(t).URL, "")
	if _, err := d.loadCodeAssist(context.Background(), "ag-1", fakeToken("access")); err == nil {
		t.Fatal("truncated loadCodeAssist body must surface as an error")
	}
}
