package opencodezen

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-127: the env block rides in the FIRST system message, appended to its
// text (one message, so its cache holds).
func TestEnvBlockAppendsToSystemText(t *testing.T) {
	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "hi"},
	}}
	// BP-188: the block is built per request (a real working directory — the
	// upstream 429s the literal "unknown" since 2026-10-29), so the test
	// reproduces the same body-derived value before the injection runs.
	env := zenEnvironmentFor(body)
	injectEnvironment(body, false)
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (no new message added)", len(msgs))
	}
	c, _ := msgs[0]["content"].(string)
	if c != "be brief\n\n"+env {
		t.Fatalf("system content = %q", c)
	}
	if _, isUser := msgs[1]["content"].(string); !isUser || msgs[1]["role"] != "user" {
		t.Fatalf("user message mangled: %v", msgs[1])
	}
}

// A request that already carries an env block is left alone; `<ENV>` and
// `working directory:` do not count (upstream checks are exact).
func TestEnvBlockSkippedWhenPresent(t *testing.T) {
	// Any env block counts as present — the request's own directory rides in
	// it and must not be touched (BP-188: the block now carries a real path,
	// but the skip rule is about the request, not the value).
	const block = "<env>\n  Working directory: /tmp/some-project\n</env>"
	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "prompt\n\n" + block},
	}}
	injectEnvironment(body, false)
	msgs := body["messages"].([]map[string]any)
	if c, _ := msgs[0]["content"].(string); c != "prompt\n\n"+block {
		t.Fatalf("content changed: %q", c)
	}
	body2 := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "<ENV>\n  Working directory: x\n</ENV>"},
	}}
	injectEnvironment(body2, false)
	msgs2 := body2["messages"].([]map[string]any)
	if c, _ := msgs2[0]["content"].(string); !zenHasEnvironment.MatchString(c) {
		t.Fatalf("exact-case env must still count as present, got %q", c)
	}
}

// Array-shaped system content gets the block as one more text part, and a
// request with no system message at all gets one put first.
func TestEnvBlockPartsAndUnshift(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "be brief"}}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	env := zenEnvironmentFor(body)
	injectEnvironment(body, false)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	parts := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if txt, _ := parts[1].(map[string]any)["text"].(string); txt != env {
		t.Fatalf("appended part = %q", txt)
	}
	none := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	noneEnv := zenEnvironmentFor(none)
	injectEnvironment(none, false)
	l := none["messages"].([]any)
	if len(l) != 2 {
		t.Fatalf("messages = %d, want 2", len(l))
	}
	first := l[0].(map[string]any)
	if first["role"] != "system" || first["content"] != noneEnv {
		t.Fatalf("unshifted system = %v", first)
	}
}

// Responses lane: the block rides in instructions.
func TestEnvBlockResponsesInstructions(t *testing.T) {
	body := map[string]any{"instructions": "be brief"}
	env := zenEnvironmentFor(body)
	injectEnvironment(body, true)
	if ins, _ := body["instructions"].(string); ins != "be brief\n\n"+env {
		t.Fatalf("instructions = %q", ins)
	}
	empty := map[string]any{}
	emptyEnv := zenEnvironmentFor(empty)
	injectEnvironment(empty, true)
	if ins, _ := empty["instructions"].(string); ins != emptyEnv {
		t.Fatalf("empty instructions = %q", ins)
	}
}

// BP-127: each gate stand-in declares the parameter OpenCode's own
// declaration leads with, additionalProperties off.
func TestGateToolSchemas(t *testing.T) {
	for _, name := range gateToolNames {
		schema := gateToolSchema(name)
		props, _ := schema["properties"].(map[string]any)
		if len(props) != 1 {
			t.Fatalf("%s properties = %v, want exactly one", name, props)
		}
		if _, ok := props[gateToolParam[name]]; !ok {
			t.Fatalf("%s schema missing param %q: %v", name, gateToolParam[name], props)
		}
	}
	got := gateToolChat("bash")
	fn := got["function"].(map[string]any)
	raw, _ := json.Marshal(fn["parameters"])
	if string(raw) != `{"additionalProperties":false,"properties":{"command":{"type":"string"}},"type":"object"}` {
		t.Fatalf("bash chat schema = %s", raw)
	}
}

// Stale configs still naming the pre-BP-127 /zen base route to the inference
// API, which is where the free lane answers since mid-October 2026.
func TestLegacyBaseRoutesToInference(t *testing.T) {
	for _, old := range []string{"https://opencode.ai/zen", "https://opencode.ai/zen/", "https://opencode.ai/zen/v1"} {
		d := New(configForBase(old), nil)
		if got := d.base(); got != zenUpstreamBase {
			t.Fatalf("base(%q) = %q, want %q", old, got, zenUpstreamBase)
		}
	}
	d := New(configForBase("https://opencode.ai/inference/v1"), nil)
	if got := d.base(); got != "https://opencode.ai/inference/v1" {
		t.Fatalf("explicit base = %q, want it kept", got)
	}
	empty := New(configForBase(""), nil)
	if got := empty.base(); got != zenUpstreamBase {
		t.Fatalf("empty base = %q, want default", got)
	}
}

func configForBase(base string) config.OpenCodeZenSettings {
	return config.OpenCodeZenSettings{Enabled: true, BaseURL: base}
}

// Wiring-level: buildWire must inject the env block into an existing system
// message (the helper tests above exercise injectEnvironment directly; this
// one goes through the entry the chat lane actually ships).
func TestBuildChatWireCarriesEnvBlock(t *testing.T) {
	d := New(configForBase("https://opencode.ai/inference/v1"), nil)
	call := driver.ChatCall{UpstreamModel: "muse-spark-1.3-contributor-free", Request: &ir.ChatRequest{
		Model: "muse-spark-1.3-contributor-free",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "be brief"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
	}}
	raw, err := d.buildWire(call)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, m := range body.Messages {
		if m.Role != "system" {
			continue
		}
		// The wire may carry the text as a JSON string or as content parts;
		// json.Marshal HTML-escapes <>, so decode before matching.
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			if zenHasEnvironment.MatchString(str) {
				return
			}
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				if zenHasEnvironment.MatchString(p.Text) {
					return
				}
			}
		}
	}
	t.Fatal("buildWire output carries no env block in any system message")
}

// The gate set is OpenCode's five-tool core, not the old bash/read pair.
func TestGateToolNamesAreTheFiveCore(t *testing.T) {
	want := map[string]bool{"bash": true, "edit": true, "glob": true, "grep": true, "read": true}
	got := map[string]bool{}
	for _, n := range gateToolNames {
		got[n] = true
	}
	if len(got) != 5 {
		t.Fatalf("gateToolNames = %v, want the five-tool core set", got)
	}
	for n := range want {
		if !got[n] {
			t.Fatalf("gateToolNames missing %q: %v", n, got)
		}
	}
}
