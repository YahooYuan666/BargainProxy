package opencodezen

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-219: the first system message is COMPOSED the way the desktop composes
// it — OpenCode's harness first, then the caller's own system text, then the
// date/env/worktree tail — one message, so its cache holds.
func TestEnvBlockAppendsToSystemText(t *testing.T) {
	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "hi"},
	}}
	env := zenEnvironmentFor(body)
	injectEnvironment(body, false)
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (no new message added)", len(msgs))
	}
	c, _ := msgs[0]["content"].(string)
	if !strings.HasPrefix(c, harnessSystem) {
		t.Fatalf("system must open with the embedded harness, got %.80q", c)
	}
	if !strings.Contains(c, "be brief") {
		t.Fatalf("the caller's own system text must survive: %q", c[:200])
	}
	for _, want := range []string{"Today's date:", env, zenWorktreeNote} {
		if !strings.Contains(c, want) {
			t.Fatalf("composed system missing %q", want)
		}
	}
	if _, isUser := msgs[1]["content"].(string); !isUser || msgs[1]["role"] != "user" {
		t.Fatalf("user message mangled: %v", msgs[1])
	}
}

// A request that already carries an env block keeps its own block; the tail
// then omits the duplicate so the message carries exactly one.
func TestEnvBlockSkippedWhenPresent(t *testing.T) {
	const block = "<env>\n  Working directory: /tmp/some-project\n</env>"
	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "prompt\n\n" + block},
	}}
	injectEnvironment(body, false)
	msgs := body["messages"].([]map[string]any)
	c, _ := msgs[0]["content"].(string)
	if !strings.HasPrefix(c, harnessSystem) || !strings.Contains(c, block) {
		t.Fatalf("caller env must survive composition: %.80q", c)
	}
	if own := zenEnvironmentFor(body); strings.Contains(c, own) {
		t.Fatalf("a second env block was appended:\n...%s", c[max(0, len(c)-300):])
	}
}

// Array-shaped system content is flattened into the composition, and a request
// with no system message at all gets one put first.
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
	c, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(c, harnessSystem) || !strings.Contains(c, "be brief") || !strings.Contains(c, env) {
		t.Fatalf("composed content = %.120q", c)
	}
	none := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	noneEnv := zenEnvironmentFor(none)
	injectEnvironment(none, false)
	l := none["messages"].([]any)
	if len(l) != 2 {
		t.Fatalf("messages = %d, want 2", len(l))
	}
	first := l[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("unshifted role = %v", first["role"])
	}
	if c, _ := first["content"].(string); !strings.HasPrefix(c, harnessSystem) || !strings.Contains(c, noneEnv) {
		t.Fatalf("unshifted system = %.120q", c)
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

// BP-127, reshaped by BP-219: the gate stand-ins are the desktop's OWN
// declarations (captured from a live 2.2.2 request), each carrying its real
// parameters — shell's command/workdir/timeout among them. The 1.19-era
// one-property stubs are what the current gate refuses.
func TestGateToolSchemas(t *testing.T) {
	chat, responses := coreToolDefs()
	if len(chat) == 0 || len(responses) == 0 {
		t.Fatal("embedded core tool definitions failed to load")
	}
	for _, name := range gateToolNames {
		ct, ok := chat[name]
		if !ok {
			t.Fatalf("chat definition missing for %q", name)
		}
		fn, _ := ct["function"].(map[string]any)
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if len(props) < 1 {
			t.Fatalf("%s chat schema has no properties: %v", name, params)
		}
		if _, ok := props["command"]; name == "shell" && !ok {
			t.Fatalf("shell schema must declare command: %v", props)
		}
		if _, ok := props["path"]; name == "read" && !ok {
			t.Fatalf("read schema must declare path: %v", props)
		}
		if rt, ok := responses[name]; !ok || toolNameOf(rt) != name {
			t.Fatalf("responses definition missing/mismamed for %q", name)
		}
	}
	if _, ok := chat["bash"]; ok {
		t.Fatal("2.2.x has no bash tool; the 1.x name must not come back")
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

// The gate set is the desktop's five-tool core (2.2.x: shell, not bash).
func TestGateToolNamesAreTheFiveCore(t *testing.T) {
	want := map[string]bool{"shell": true, "edit": true, "glob": true, "grep": true, "read": true}
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
