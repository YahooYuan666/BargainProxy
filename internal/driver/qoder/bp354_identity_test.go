// BP-354 (magpie-community/plugins de308730 / 1e4844b3): two identity
// sentences in a system prompt make Qoder refuse the request — one with 400
// "Cannot find model" on any model, one with a 10605 queue that never moves.
// Only the SYSTEM text is rewritten; the user's own words go as written.
package qoder

import (
	"strings"
	"testing"
)

const (
	ccWireLine  = "You are Claude Code, Anthropic's official CLI for Claude."
	dshWireLine = "You are an AI agent powered by DeepSeek Harness."
)

// The reporter's literal line (tweet 2125095548424511906) must be swapped, and
// the rest of the prompt must survive verbatim.
func TestBP354ClaudeCodeLineSwapped(t *testing.T) {
	in := "x-anthropic-billing-header: cc_version=2.1.80;\n\n" +
		ccWireLine + "\n" +
		"You are an interactive agent that helps users with software engineering tasks."
	got := sanitizeSystemText(in)
	if strings.Contains(got, "You are Claude Code") {
		t.Fatalf("Claude Code's identity line survived: %q", got)
	}
	if !strings.Contains(got, agentSDKLine) {
		t.Fatalf("the Agent SDK line is missing: %q", got)
	}
	if !strings.Contains(got, "x-anthropic-billing-header: cc_version=2.1.80;") {
		t.Fatalf("the rest of the prompt was damaged: %q", got)
	}
	if !strings.Contains(got, "You are an interactive agent that helps users with software engineering tasks.") {
		t.Fatalf("the following sentence was lost: %q", got)
	}
}

// Claude Code's SDK variant (the same sentence plus ", running within the
// Claude Agent SDK.") is swapped the same way.
func TestBP354SDKVariantSwapped(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	if got := sanitizeSystemText(in); got != agentSDKLine {
		t.Fatalf("got %q, want the SDK line", got)
	}
}

// White space varies between client versions and must not defeat the match.
func TestBP354WhiteSpaceTolerant(t *testing.T) {
	in := "You are Claude Code,\nAnthropic's  official CLI for Claude.\n\nBe brief."
	got := sanitizeSystemText(in)
	if strings.Contains(got, "You are Claude Code") {
		t.Fatalf("white-spaced line survived: %q", got)
	}
	if !strings.HasPrefix(got, agentSDKLine) || !strings.Contains(got, "Be brief.") {
		t.Fatalf("got %q", got)
	}
}

// Prompts that already say the right thing, or merely mention Claude Code, are
// left alone — a substring match would corrupt them.
func TestBP354UnrelatedPromptsUntouched(t *testing.T) {
	for _, s := range []string{
		agentSDKLine,
		"You are a coding agent.",
		"Claude Code is Anthropic's official CLI for Claude.",
		"You are a helpful assistant.",
	} {
		if got := sanitizeSystemText(s); got != s {
			t.Fatalf("sanitizeSystemText(%q) = %q — must be untouched", s, got)
		}
	}
}

// The DSH opener: 48 characters that put the request in a queue nothing
// drains. A comma before "powered by" passes and keeps the name.
func TestBP354HarnessOpenerGetsComma(t *testing.T) {
	got := sanitizeSystemText(dshWireLine + "\n\nBe brief.")
	want := "You are an AI agent, powered by DeepSeek Harness.\n\nBe brief."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Other harness verbs are the same sentence family.
func TestBP354HarnessOtherVerbs(t *testing.T) {
	cases := map[string]string{
		"You are an AI agent running on Foo.":  "You are an AI agent, running on Foo.",
		"You are a coding assistant built on Bar.": "You are a coding assistant, built on Bar.",
	}
	for in, want := range cases {
		if got := sanitizeSystemText(in); got != want {
			t.Fatalf("sanitizeSystemText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Idempotent: a rewrite that runs twice (a retry path, a hot reload) must not
// add a second comma or a second swap.
func TestBP354Idempotent(t *testing.T) {
	once := sanitizeSystemText(ccWireLine + "\n" + dshWireLine)
	twice := sanitizeSystemText(once)
	if once != twice {
		t.Fatalf("not idempotent: %q → %q", once, twice)
	}
}

// A sentence that merely mentions the harness is not the opener (magpie's test
// pins this: "This agent is powered by DeepSeek Harness." must not match).
func TestBP354NonOpenerMentionsUntouched(t *testing.T) {
	for _, s := range []string{
		"This agent is powered by DeepSeek Harness.",
		"You are a Qoder agent. Use the instructions below.",
	} {
		if got := sanitizeSystemText(s); got != s {
			t.Fatalf("sanitizeSystemText(%q) = %q — must be untouched", s, got)
		}
	}
}

// buildQoderMessages must rewrite ONLY the system turn. A user quoting either
// sentence has to reach the model verbatim.
func TestBP354OnlySystemTurnRewritten(t *testing.T) {
	incoming := []map[string]interface{}{
		{"role": "system", "content": ccWireLine},
		{"role": "user", "content": ccWireLine + " hi"},
	}
	msgs := buildQoderMessages(incoming, "", false)
	if len(msgs) < 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if strings.Contains(msgs[0].Content, "You are Claude Code") {
		t.Fatalf("the system turn kept the refusal line: %q", msgs[0].Content)
	}
	// A user turn carries its text in Parts (buildUserMessage), not Content.
	var userText string
	for _, p := range msgs[1].Contents {
		userText += p.Text
	}
	if !strings.Contains(userText, ccWireLine) {
		t.Fatalf("the user's own words were edited: %q", userText)
	}
}

// A `developer` role arrives folded to `system`, and must be rewritten too —
// Qoder CN queues on the opener wherever it sits in the system text.
func TestBP354DeveloperRoleRewritten(t *testing.T) {
	incoming := []map[string]interface{}{
		{"role": "developer", "content": dshWireLine},
		{"role": "user", "content": "hi"},
	}
	msgs := buildQoderMessages(incoming, "", false)
	if len(msgs) == 0 {
		t.Fatal("no messages built")
	}
	if strings.Contains(msgs[0].Content, dshWireLine) {
		t.Fatalf("developer-role opener survived: %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "powered by DeepSeek Harness") {
		t.Fatalf("the harness name was removed instead of comma-separated: %q", msgs[0].Content)
	}
}
