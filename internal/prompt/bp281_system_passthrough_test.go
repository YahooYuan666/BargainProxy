package prompt

import (
	"strings"
	"testing"
)

// BP-281: the caller's system prompt must survive the workbuddy channel.
//
// These tests pin the four primitives the driver composes: SplitSystem and
// MoveSystemToUser are the 11128 rescue rungs (in that order — keeping the text
// in the system slot beats relocating it), EnsureSystemFirst is the channel
// check's hard requirement, and Append is the non-destructive third mode.

func sysMsg(text string) map[string]any  { return map[string]any{"role": "system", "content": text} }
func userMsg(text string) map[string]any { return map[string]any{"role": "user", "content": text} }
func asstMsg(text string) map[string]any { return map[string]any{"role": "assistant", "content": text} }
func textOf(m map[string]any) string     { s, _ := m["content"].(string); return s }

// --- SplitSystem -----------------------------------------------------------

func TestSplitSystemCutsAtSentenceBoundaries(t *testing.T) {
	in := []map[string]any{
		sysMsg("You are Claude Code, Anthropic's official CLI. Always answer briefly. Never invent file paths."),
		userMsg("hi"),
	}
	out, ok := SplitSystem(in)
	if !ok {
		t.Fatal("a three-sentence system must be splittable")
	}
	if len(out) != 4 { // 3 sentences + the user turn
		t.Fatalf("got %d messages, want 4: %v", len(out), out)
	}
	for i := 0; i < 3; i++ {
		if out[i]["role"] != "system" {
			t.Fatalf("part %d role = %v, want system (the channel must not change)", i, out[i]["role"])
		}
	}
	// The text must be preserved verbatim: same characters, only re-cut.
	joined := textOf(out[0]) + " " + textOf(out[1]) + " " + textOf(out[2])
	want := textOf(in[0])
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(want), " ") {
		t.Fatalf("text changed:\n got %q\nwant %q", joined, want)
	}
	// The whole run must be broken: no single message may still carry the
	// original continuous string, or the edge's match would survive.
	for _, m := range out[:3] {
		if textOf(m) == want {
			t.Fatal("the original continuous run survived in one message")
		}
	}
}

func TestSplitSystemLeavesOtherMessagesAlone(t *testing.T) {
	in := []map[string]any{
		sysMsg("One. Two."),
		userMsg("question"),
		asstMsg("earlier answer"),
	}
	out, ok := SplitSystem(in)
	if !ok {
		t.Fatal("want split")
	}
	if len(out) != 4 {
		t.Fatalf("got %d messages, want 4", len(out))
	}
	if out[2]["content"] != "question" || out[3]["content"] != "earlier answer" {
		t.Fatalf("non-system messages were touched: %v", out)
	}
}

func TestSplitSystemRefusesSingleSentence(t *testing.T) {
	in := []map[string]any{sysMsg("You are Claude Code."), userMsg("hi")}
	if _, ok := SplitSystem(in); ok {
		t.Fatal("a single-sentence system must not split: cutting it in half breaks the instruction")
	}
}

func TestSplitSystemRefusesWithoutSystem(t *testing.T) {
	in := []map[string]any{userMsg("hi"), asstMsg("hello")}
	if _, ok := SplitSystem(in); ok {
		t.Fatal("no system message means nothing to split")
	}
}

func TestSplitSystemOnlyCutsTheLongest(t *testing.T) {
	in := []map[string]any{
		sysMsg("Short."),
		sysMsg("First sentence here. Second sentence here."),
		userMsg("hi"),
	}
	out, ok := SplitSystem(in)
	if !ok {
		t.Fatal("want split")
	}
	if textOf(out[0]) != "Short." {
		t.Fatalf("the short system message was modified: %q", textOf(out[0]))
	}
	if len(out) != 4 { // short + 2 parts + user
		t.Fatalf("got %d messages, want 4: %v", len(out), out)
	}
}

func TestSplitSentencesKeepsAbbreviationsWhole(t *testing.T) {
	got := SplitSentences("Version 3.5 is out. See example.com for details.")
	if len(got) != 2 {
		t.Fatalf("got %d parts, want 2: %v", len(got), got)
	}
	if got[0] != "Version 3.5 is out." {
		t.Fatalf("part 0 = %q (a decimal point must not split)", got[0])
	}
}

// --- MoveSystemToUser ------------------------------------------------------

func TestMoveSystemToUserLosesNothing(t *testing.T) {
	const original = "You are Claude Code, Anthropic's official CLI."
	in := []map[string]any{sysMsg(original), userMsg("hello")}
	out, ok := MoveSystemToUser(in, DegradedSystemPrompt)
	if !ok {
		t.Fatal("want move")
	}
	if textOf(out[0]) != DegradedSystemPrompt {
		t.Fatalf("system slot = %q, want the neutral line", textOf(out[0]))
	}
	if !strings.Contains(textOf(out[1]), original) {
		t.Fatalf("the original text was lost: %q", textOf(out[1]))
	}
	if !strings.HasSuffix(textOf(out[1]), "hello") {
		t.Fatalf("the user's own question must come after the moved text: %q", textOf(out[1]))
	}
}

func TestMoveSystemToUserRefusesMultimodalFirstUser(t *testing.T) {
	in := []map[string]any{
		sysMsg("rules"),
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "look"}}},
	}
	if _, ok := MoveSystemToUser(in, DegradedSystemPrompt); ok {
		t.Fatal("prepending text into parts would reorder them; must refuse")
	}
}

func TestMoveSystemToUserRefusesWithoutUser(t *testing.T) {
	in := []map[string]any{sysMsg("rules"), asstMsg("hello")}
	if _, ok := MoveSystemToUser(in, DegradedSystemPrompt); ok {
		t.Fatal("no user turn to move into; must refuse")
	}
}

// --- EnsureSystemFirst -----------------------------------------------------

func TestEnsureSystemFirstFillsMissingSystem(t *testing.T) {
	out := EnsureSystemFirst([]map[string]any{userMsg("hi")}, "gateway line")
	if len(out) != 2 || out[0]["role"] != "system" || textOf(out[0]) != "gateway line" {
		t.Fatalf("got %v, want a system line prepended", out)
	}
	if textOf(out[1]) != "hi" {
		t.Fatalf("the user turn was modified: %v", out[1])
	}
}

func TestEnsureSystemFirstLeavesExistingSystemAlone(t *testing.T) {
	in := []map[string]any{sysMsg("mine"), userMsg("hi")}
	out := EnsureSystemFirst(in, "gateway line")
	if len(out) != 2 || textOf(out[0]) != "mine" {
		t.Fatalf("an existing system message must not be replaced: %v", out)
	}
}

func TestEnsureSystemFirstNormalizesDeveloper(t *testing.T) {
	in := []map[string]any{{"role": "developer", "content": "mine"}, userMsg("hi")}
	out := EnsureSystemFirst(in, "gateway line")
	if out[0]["role"] != "system" || textOf(out[0]) != "mine" {
		t.Fatalf("developer must become system with its text intact: %v", out[0])
	}
	if len(out) != 2 {
		t.Fatalf("normalization must not add a message: %v", out)
	}
}

func TestEnsureSystemFirstDoesNotMutateInput(t *testing.T) {
	in := []map[string]any{{"role": "developer", "content": "mine"}, userMsg("hi")}
	_ = EnsureSystemFirst(in, "gateway line")
	if in[0]["role"] != "developer" {
		t.Fatal("the caller's slice must not be mutated in place")
	}
}

// --- Append ----------------------------------------------------------------

func TestAppendKeepsCallerText(t *testing.T) {
	const mine = "You are Claude Code."
	out := Append([]map[string]any{sysMsg(mine), userMsg("hi")}, "gateway line")
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3", len(out))
	}
	if textOf(out[0]) != mine {
		t.Fatalf("the caller's system text must stay first and unchanged: %q", textOf(out[0]))
	}
	if textOf(out[1]) != "gateway line" {
		t.Fatalf("gateway line must sit right after the leading system run: %q", textOf(out[1]))
	}
}

func TestAppendDoesNotJumpMidConversationSystem(t *testing.T) {
	in := []map[string]any{
		sysMsg("first"),
		userMsg("q"),
		sysMsg("mid-conversation note"),
	}
	out := Append(in, "gateway line")
	if len(out) != 4 || textOf(out[1]) != "gateway line" {
		t.Fatalf("append point must be after the LEADING run only: %v", out)
	}
	if textOf(out[3]) != "mid-conversation note" {
		t.Fatalf("a mid-conversation system message must not move: %v", out)
	}
}
