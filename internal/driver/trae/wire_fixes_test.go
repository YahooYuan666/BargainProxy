package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TRAE-7/9/10 red-first tests (BP-98).

// TestImagePartPassesThrough (TRAE-7): an image content part must reach the
// upstream as an OpenAI image_url part — it used to be silently dropped and
// the model answered about "the other image" (BP-70 class of incident, but
// invisible: a 200 with confident nonsense).
func TestImagePartPassesThrough(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
				{Kind: "text", Text: "what is this?"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: "aWNvbg=="},
			}}},
			Stream: true,
		},
	}
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in body")
	}
	m0, _ := msgs[0].(map[string]any)
	parts, _ := m0["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %d, want 2 (text + image)", len(parts))
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part type = %v, want image_url", img["type"])
	}
	iu, _ := img["image_url"].(map[string]any)
	if iu == nil || !strings.Contains(fmt.Sprint(iu["url"]), "data:image/png;base64,aWNvbg==") {
		t.Fatalf("image url = %v", iu)
	}
}

// TestDetectErrorNestedShapes (TRAE-9): a 200 whose body is a JSON error in
// one of the nested shapes must classify as a business error, not fall into
// the legacy-chunk parser and end as "stream ended without terminal signal"
// (ClassTransport — a business refusal scored as a network problem).
func TestDetectErrorNestedShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"wrapped", `{"error":{"code":4001,"message":"no"}}`, 4001},
		{"bytedance", `{"ResponseMetadata":{"Error":{"Code":"1005","Message":"plan"}}}`, 1005},
	}
	for _, tc := range cases {
		code, msg, ok := detectError(tc.body)
		if !ok {
			t.Fatalf("%s: detectError missed the nested shape", tc.name)
		}
		if code != tc.code {
			t.Fatalf("%s: code = %d, want %d", tc.name, code, tc.code)
		}
		if msg == "" {
			t.Fatalf("%s: message lost", tc.name)
		}
	}
	// A plain content payload is still not an error.
	if _, _, ok := detectError(`{"response":"hello"}`); ok {
		t.Fatal("a content payload must not read as an error")
	}
}

// Test200JSONErrorBodyClassifiesBusiness (TRAE-9 end to end): the upstream
// answering JSON (not event-stream) with a nested error must surface the
// business class, not transport.
func Test200JSONErrorBodyClassifiesBusiness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":4001,"message":"cannot unmarshal"}}`)
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "glm-5.2",
		Request: &ir.ChatRequest{
			Model:    "glm-5.2",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("expected uperr, got %v", err)
	}
	if ue.Class == "transport" {
		t.Fatalf("a nested business error must not read as transport: %v", err)
	}
	if ue.Code != "4001" {
		t.Fatalf("code = %q, want 4001", ue.Code)
	}
}

// TestBodyCarriesSessionAndRequestID (TRAE-10): the chat body carries
// session_id (stable per conversation, account-scoped) and request_id (fresh
// per attempt) so the upstream usage page keeps one multi-step conversation
// in one row instead of scattering it.
func TestBodyCarriesSessionAndRequestID(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	mkCall := func(sessionKey string) driver.ChatCall {
		return driver.ChatCall{
			Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
			UpstreamModel: "glm-5.2",
			Request: &ir.ChatRequest{
				Model:      "glm-5.2",
				SessionKey: sessionKey,
				Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
				Stream:     true,
			},
		}
	}
	if _, err := d.Stream(context.Background(), mkCall("sess-abc"), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream 1: %v", err)
	}
	if _, err := d.Stream(context.Background(), mkCall("sess-abc"), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream 2: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d", len(bodies))
	}
	s1, _ := bodies[0]["session_id"].(string)
	s2, _ := bodies[1]["session_id"].(string)
	if s1 == "" || s1 != s2 {
		t.Fatalf("session_id must be stable across calls of one conversation, got %q vs %q", s1, s2)
	}
	r1, _ := bodies[0]["request_id"].(string)
	r2, _ := bodies[1]["request_id"].(string)
	if r1 == "" || r2 == "" || r1 == r2 {
		t.Fatalf("request_id must be fresh per attempt, got %q vs %q", r1, r2)
	}
	// Different session keys must not share a session id.
	if _, err := d.Stream(context.Background(), mkCall("sess-other"), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream 3: %v", err)
	}
	s3, _ := bodies[2]["session_id"].(string)
	if s3 == "" || s3 == s1 {
		t.Fatalf("different conversations must map to different session ids, got %q twice", s1)
	}
}
