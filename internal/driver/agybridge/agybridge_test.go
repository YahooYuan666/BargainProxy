package agybridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-339: the child environment is the load-bearing isolation. Four pieces
// must be present (HOME/USERPROFILE/GEMINI_HOME redirect + force-file token
// storage) and the host identity leaks must be stripped.
func TestChildEnvIsolation(t *testing.T) {
	t.Setenv("HOME", `C:\Users\host`)
	t.Setenv("USERPROFILE", `C:\Users\host`)
	t.Setenv("GEMINI_API_KEY", "leak-me")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", `C:\leak.json`)
	env := childEnv(`D:\bp\agybridge\acc1\home`)

	joined := strings.Join(env, "\n")
	mustHave := []string{
		"HOME=D:\\bp\\agybridge\\acc1\\home",
		"USERPROFILE=D:\\bp\\agybridge\\acc1\\home",
		"GEMINI_HOME=D:\\bp\\agybridge\\acc1\\home\\.gemini",
		"AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1",
		"AGY_CLI_DISABLE_AUTO_UPDATE=true",
	}
	for _, want := range mustHave {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q", want)
		}
	}
	mustNot := []string{"GEMINI_API_KEY=", "GOOGLE_APPLICATION_CREDENTIALS=", "HOME=C:\\Users\\host", "USERPROFILE=C:\\Users\\host"}
	for _, bad := range mustNot {
		if strings.Contains(joined, bad) {
			t.Errorf("env leaked %q", bad)
		}
	}
}

// BP-339: accountHome derives/provisions the isolated home from creds, or
// falls back to accounts_dir/<uid>/home.
func TestAccountHome(t *testing.T) {
	dir := t.TempDir()
	d2 := newTestDriver(dir)

	acc := &model.Account{ID: "agybridge:abc", Channel: model.ChannelAgyBridge, UID: "u1"}
	home, err := d2.accountHome(acc)
	if err != nil {
		t.Fatalf("accountHome: %v", err)
	}
	want := filepath.Join(dir, "u1", "home")
	if home != want {
		t.Fatalf("home = %q, want %q", home, want)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Fatalf("home not provisioned: %v", err)
	}

	// CredsJSON with an explicit home wins (re-import keeps the same home) —
	// accountHome does NOT create an out-of-tree home, it only reports it.
	acc2 := &model.Account{ID: "agybridge:abc", Channel: model.ChannelAgyBridge,
		CredsJSON: mustJSON(t, Creds{Email: "a@b.c", Home: filepath.Join(dir, "custom-home")})}
	home2, err := d2.accountHome(acc2)
	if err != nil {
		t.Fatalf("accountHome(creds): %v", err)
	}
	if home2 != filepath.Join(dir, "custom-home") {
		t.Fatalf("creds home ignored: %q", home2)
	}
}

// The prompt body is USER TEXT ONLY: the system prompt travels in ACP's own
// `_meta.antigravity.systemPrompt.append` field (upstream worker.ts), and the
// session already holds the history.
func TestBuildPromptSendsOnlyNewTurn(t *testing.T) {
	req := &ir.ChatRequest{Messages: []ir.Message{
		{Role: "system", Content: []ir.ContentPart{{Kind: "text", Text: "You are a writer."}}},
		{Role: "user", Content: []ir.ContentPart{{Kind: "text", Text: "第一轮问题"}}},
		{Role: "assistant", Content: []ir.ContentPart{{Kind: "text", Text: "第一轮回答"}}},
		{Role: "user", Content: []ir.ContentPart{{Kind: "text", Text: "第二轮问题"}}},
	}}
	p := buildPrompt(req)
	if !strings.Contains(p, "第二轮问题") {
		t.Error("the new user turn was dropped")
	}
	if strings.Contains(p, "第一轮问题") || strings.Contains(p, "第一轮回答") {
		t.Error("history must not be replayed into the prompt (the session already holds it)")
	}
	if strings.Contains(p, "You are a writer.") {
		t.Error("system prompt must not be inlined into the prompt body (it has its own ACP field)")
	}
	if got := systemTextOf(req); !strings.Contains(got, "You are a writer.") {
		t.Errorf("systemTextOf = %q, want the system text for the _meta field", got)
	}
}

// A resumed session with no fresh user text still needs a non-empty prompt.
func TestBuildPromptNoNewTurn(t *testing.T) {
	req := &ir.ChatRequest{Messages: []ir.Message{
		{Role: "user", Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		{Role: "assistant", Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
	}}
	if got := buildPrompt(req); got != "(continue)" {
		t.Fatalf("buildPrompt = %q, want the continuation marker", got)
	}
}

func TestParseUpdateChunks(t *testing.T) {
	frame := rpcMsg{Params: mustJSONRaw(t, map[string]any{
		"sessionId": "s1",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "hello"},
		},
	})}
	kind, text := parseUpdate(frame)
	if kind != "agent_message_chunk" || text != "hello" {
		t.Fatalf("got %q/%q", kind, text)
	}
	thought := rpcMsg{Params: mustJSONRaw(t, map[string]any{
		"sessionId": "s1",
		"update": map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": "hmm"},
		},
	})}
	kind, text = parseUpdate(thought)
	if kind != "agent_thought_chunk" || text != "hmm" {
		t.Fatalf("thought got %q/%q", kind, text)
	}
}

// BP-339: probe reports token-file presence without ever reading it.
func TestProbeTokenFile(t *testing.T) {
	dir := t.TempDir()
	d := newTestDriver(dir)
	acc := &model.Account{ID: "agybridge:x", Channel: model.ChannelAgyBridge, UID: "u9"}
	home, _ := d.accountHome(acc)

	res, err := d.Probe(testCtx(), acc)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(res.Note, "尚未登录") {
		t.Fatalf("expected no-token note, got %q", res.Note)
	}

	tokDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokDir, "antigravity-oauth-token"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = d.Probe(testCtx(), acc)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(res.Note, "已登录") {
		t.Fatalf("expected token-present note, got %q", res.Note)
	}
}

// normalizeImport panel path: email-keyed stable ID, home passed through.
func TestNormalizeImportAgyBridge(t *testing.T) {
	acc, err := normalizeAgyBridgeForTest("writer@gmail.com", "D:\\homes\\w1")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(acc.ID, "agybridge:") || acc.ID == "agybridge:" {
		t.Fatalf("ID = %q", acc.ID)
	}
	var c Creds
	if err := json.Unmarshal(acc.CredsJSON, &c); err != nil {
		t.Fatal(err)
	}
	if c.Email != "writer@gmail.com" || c.Home != "D:\\homes\\w1" {
		t.Fatalf("creds = %+v", c)
	}
	if acc.Nickname != "writer@gmail.com" {
		t.Fatalf("nickname = %q", acc.Nickname)
	}
}

// --- helpers ---

func newTestDriver(accountsDir string) *Driver {
	return &Driver{cfg: testSettings(accountsDir)}
}

func testSettings(accountsDir string) config.AgyBridgeSettings {
	return config.AgyBridgeSettings{AccountsDir: accountsDir, MaxInFlight: 1}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSONRaw(t *testing.T, v any) json.RawMessage {
	return json.RawMessage(mustJSON(t, v))
}

func testCtx() context.Context { return context.Background() }

// normalizeAgyBridgeForTest exercises the agybridge credential shape (the
// panel's normalizeImport lives in internal/panel; the shape contract is
// duplicated here to stay cycle-free).
func normalizeAgyBridgeForTest(email, home string) (*model.Account, error) {
	raw, _ := json.Marshal(map[string]string{"email": email, "home": home})
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	e := ""
	if v, ok := m["email"].(string); ok {
		e = v
	}
	if e == "" {
		return nil, os.ErrInvalid
	}
	h := ""
	if v, ok := m["home"].(string); ok {
		h = v
	}
	creds, _ := json.Marshal(map[string]string{"email": e, "home": h})
	return &model.Account{
		ID:       model.ChannelAgyBridge + ":testhash",
		Channel:  model.ChannelAgyBridge,
		Nickname: e,
		CredsJSON: creds,
	}, nil
}


// BP-341: thinking depth must follow the same resolution the antigravity direct
// driver uses (explicit level → budget band → reasoning_effort), because the
// agy session exposes exactly low|medium|high|xhigh and the in-app picker shows
// 低/中/高/极高.
func TestThinkingLevelOf(t *testing.T) {
	cases := []struct {
		name string
		req  *ir.ChatRequest
		want string
	}{
		{"nil request", nil, ""},
		{"no thinking", &ir.ChatRequest{}, ""},
		{"explicit level", &ir.ChatRequest{Thinking: &ir.ThinkingConfig{Enabled: true, Level: "xhigh"}}, "xhigh"},
		{"uppercase level", &ir.ChatRequest{Thinking: &ir.ThinkingConfig{Enabled: true, Level: "HIGH"}}, "high"},
		{"budget low", &ir.ChatRequest{Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 2048}}, "low"},
		{"budget medium", &ir.ChatRequest{Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 8000}}, "medium"},
		{"budget high", &ir.ChatRequest{Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 40000}}, "high"},
		{"requested effort", &ir.ChatRequest{RequestedEffort: "medium"}, "medium"},
		{"thinking disabled falls back to effort", &ir.ChatRequest{Thinking: &ir.ThinkingConfig{Enabled: false}, RequestedEffort: "low"}, "low"},
	}
	for _, c := range cases {
		if got := thinkingLevelOf(c.req); got != c.want {
			t.Errorf("%s: thinkingLevelOf = %q, want %q", c.name, got, c.want)
		}
	}
}

