package opencodezen

import (
	"strings"
	"testing"
)

func TestInvertedTimestampEndsWithFfe(t *testing.T) {
	hex := invertedTimestampHex(1_791_895_680_000)
	if hex[9:12] != "ffe" {
		t.Fatalf("inverted timestamp = %s, want a …ffe tail (official counter=1)", hex)
	}
	if !isDescendingTimePart(hex) {
		t.Fatalf("%s must pass the descending check", hex)
	}
}

func TestCanonicalSessionShapeAndStability(t *testing.T) {
	a := deriveRequestIDs([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	b := deriveRequestIDs([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	if a.session != b.session {
		t.Fatalf("same conversation must map to the same session: %s vs %s", a.session, b.session)
	}
	if !isCanonicalSession(a.session) {
		t.Fatalf("session shape wrong: %s", a.session)
	}
	if a.session[13:16] != "ffe" {
		t.Fatalf("minted session must carry the ffe tail: %s", a.session)
	}
	if !strings.HasPrefix(a.request, "msg_") {
		t.Fatalf("request id shape: %s", a.request)
	}
	// A pre-shaped canonical session passes through untouched.
	fixed := "ses_ebb8a5ce7ffe3IcquxCbUTQMpq"
	ids := deriveRequestIDs([]byte(`{"messages":[{"role":"user","content":"` + fixed + `"}]}`))
	// The content differs from the fixed id, so a new session is minted — but
	// canonicalSessionID(fixed) directly must return fixed.
	if got := canonicalSessionID(fixed); got != fixed {
		t.Fatalf("canonical pass-through broken: %s", got)
	}
	_ = ids
}

func TestDisguiseHeadersComplete(t *testing.T) {
	ids := deriveRequestIDs([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	headers := disguiseHeaders(ids, "chat")
	names := map[string]bool{}
	for _, h := range headers {
		names[h[0]] = true
	}
	for _, want := range []string{"user-agent", "x-opencode-client", "x-opencode-session", "x-session-affinity", "x-session-id", "x-opencode-request", "x-opencode-project", "authorization"} {
		if !names[want] {
			t.Fatalf("missing header %s", want)
		}
	}
	if ua := openCodeUserAgent("chat"); !strings.HasPrefix(ua, "opencode/") {
		t.Fatalf("UA = %q", ua)
	}
}

// TestUserAgentMatchesOfficialShape pins BP-127: 2.x sends ONE UA on every
// protocol — `opencode/latest/<version>/cli` — and the old 1.19.0 shape is
// exactly what Zen's free tier started answering 429 FreeUsageLimitError to
// (2026-10-26), while any other string is served.
func TestUserAgentMatchesOfficialShape(t *testing.T) {
	want := "opencode/latest/" + openCodeClientVersion + "/cli"
	for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
		if got := openCodeUserAgent(protocol); got != want {
			t.Fatalf("UA(%s) = %q, want %q", protocol, got, want)
		}
	}
	if openCodeClientVersion != "2.0.25" {
		t.Fatalf("client version = %q, want the captured 2.0.25", openCodeClientVersion)
	}
}

func TestEnsureGateToolsChat(t *testing.T) {
	// No tools at all: all five injected + tool_choice none.
	body := map[string]any{"model": "x"}
	if n := ensureGateTools(body, false); n != len(gateToolNames) {
		t.Fatalf("injected = %d, want %d", n, len(gateToolNames))
	}
	tools := body["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[toolNameOf(tl.(map[string]any))] = true
	}
	if !names["bash"] || !names["read"] {
		t.Fatalf("gate tools missing: %v", names)
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("tool-less chat lane must pin tool_choice=none: %v", body["tool_choice"])
	}
	// Client brought bash: the remaining four injected; tool_choice untouched.
	body2 := map[string]any{"tools": []any{gateToolChat("bash")}, "tool_choice": "auto"}
	if n := ensureGateTools(body2, false); n != len(gateToolNames)-1 {
		t.Fatalf("second injection = %d, want %d (everything but bash)", n, len(gateToolNames)-1)
	}
	if body2["tool_choice"] != "auto" {
		t.Fatalf("existing tool_choice must not change: %v", body2["tool_choice"])
	}
}

func TestEnsureGateToolsResponses(t *testing.T) {
	// Responses lane: flat tool shape; a host "none" must be stripped, and a
	// tool-less request must NOT get tool_choice none (upstream 400).
	body := map[string]any{"tool_choice": "none"}
	ensureGateTools(body, true)
	if _, present := body["tool_choice"]; present {
		t.Fatalf("responses lane must never carry tool_choice none: %v", body["tool_choice"])
	}
	tools := body["tools"].([]any)
	if toolNameOf(tools[0].(map[string]any)) != "bash" {
		t.Fatalf("responses tools must be flat-shaped: %v", tools[0])
	}
}

func TestConversationSeedStable(t *testing.T) {
	a := conversationSeed([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`))
	b := conversationSeed([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if a != `"hi"` || b != `"hi"` {
		t.Fatalf("seed = %q / %q, want the first user message", a, b)
	}
	// Responses wire: plain-string input.
	c := conversationSeed([]byte(`{"input":"hello there"}`))
	if c != `"hello there"` {
		t.Fatalf("responses seed = %q", c)
	}
}
