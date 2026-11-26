// BP-350 (magpie 33b04b64, #7565): a tool call that names no function runs
// nowhere, so it is no call — dropped on the reply side AND from a replayed
// history, together with the tool result answering it.
//
// The failure this prevents is permanent: GLM-5.3 answered a tool_use whose
// name was "", the client replayed it as "name":"", and the upstream 400'd
// that turn and every later turn of the same conversation. No account change
// or retry could heal it.
package ir

import (
	"encoding/json"
	"testing"
)

func namelessCall(id, name, args string) ToolCall {
	return ToolCall{ID: id, Name: name, Arguments: args}
}

// The headline shape: an id and arguments but NO name is still no call.
func TestBP350NormalizeDropsNamelessKeepingIDAndArgs(t *testing.T) {
	in := []ToolCall{
		namelessCall("c1", "", `{"a":1}`),
		namelessCall("c2", "Bash", `{"command":"ls"}`),
	}
	out, changed := NormalizeToolCalls(in)
	if !changed {
		t.Fatal("a nameless call must report a change")
	}
	if len(out) != 1 || out[0].Name != "Bash" {
		t.Fatalf("got %+v, want only the named call", out)
	}
	// The survivors are CONTIGUOUS from 0 — a batch of two with the first
	// dropped must present index 0, not 1 (strict clients reject a gap).
	if out[0].ID != "c2" {
		t.Fatalf("wrong survivor: %+v", out[0])
	}
}

// The fully-empty placeholder (BP-274) is a strict subset of this rule.
func TestBP350NormalizeCoversFullyEmptyPlaceholder(t *testing.T) {
	in := []ToolCall{
		namelessCall("", "", ""),
		namelessCall("c2", "Read", `{}`),
	}
	out, changed := NormalizeToolCalls(in)
	if !changed || len(out) != 1 || out[0].Name != "Read" {
		t.Fatalf("got %+v changed=%v", out, changed)
	}
}

// A clean reply reports no change and returns the SAME slice (the caller may
// keep using it byte for byte).
func TestBP350NormalizeCleanReplyIsNoOp(t *testing.T) {
	in := []ToolCall{namelessCall("c1", "Bash", `{}`), namelessCall("c2", "Read", `{}`)}
	out, changed := NormalizeToolCalls(in)
	if changed {
		t.Fatal("a clean reply must not report a change")
	}
	if len(out) != 2 {
		t.Fatalf("clean reply lost calls: %+v", out)
	}
}

// A named but argless call is REAL (BP-274's rule survives).
func TestBP350NamedArglessCallStays(t *testing.T) {
	out, _ := NormalizeToolCalls([]ToolCall{namelessCall("c1", "Bash", "")})
	if len(out) != 1 {
		t.Fatal("a named call with no arguments must stay")
	}
}

// The reply stop reason: "all calls were nameless" leaves nothing to run, so
// the turn is an ordinary end — an agent told "stop for tools" would wait.
func TestBP350ReplyStopForNamelessOnly(t *testing.T) {
	if got := ReplyStopForNameless(StopToolUse, false, true); got != StopEnd {
		t.Fatalf("a reply whose only calls were nameless ended %v, want StopEnd", got)
	}
	if got := ReplyStopForNameless(StopToolUse, true, true); got != StopToolUse {
		t.Fatalf("a real call alongside must keep StopToolUse, got %v", got)
	}
	if got := ReplyStopForNameless(StopToolUse, true, false); got != StopToolUse {
		t.Fatalf("an untouched reply must not change, got %v", got)
	}
	// Other stop reasons are not this rule's business.
	if got := ReplyStopForNameless(StopLength, false, true); got != StopLength {
		t.Fatalf("StopLength must survive, got %v", got)
	}
}

// SplitNameless is what every protocol writer calls: the kept slice is
// contiguous and the flags say what the reply originally carried.
func TestBP350SplitNameless(t *testing.T) {
	named, nameless, kept := SplitNameless([]ToolCall{
		namelessCall("c0", "Bash", `{}`),
		namelessCall("c1", "", `{"x":1}`),
		namelessCall("c2", "Read", `{}`),
	})
	if !named || !nameless {
		t.Fatalf("flags: named=%v nameless=%v, want both true", named, nameless)
	}
	if len(kept) != 2 || kept[0].Name != "Bash" || kept[1].Name != "Read" {
		t.Fatalf("kept = %+v", kept)
	}

	named, nameless, kept = SplitNameless([]ToolCall{namelessCall("c1", "", `{}`)})
	if named || !nameless || len(kept) != 0 {
		t.Fatalf("all-nameless: named=%v nameless=%v kept=%+v", named, nameless, kept)
	}

	named, nameless, kept = SplitNameless(nil)
	if named || nameless || kept != nil {
		t.Fatalf("empty input: named=%v nameless=%v kept=%+v", named, nameless, kept)
	}
}

// The replayed history: the call goes AND the tool result answering it by id
// goes with it — leaving the result behind would produce an orphan and the
// strict upstreams reject the whole history (the exact opposite of the intent).
func TestBP350WithoutNamelessCallsDropsPair(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "do it"}}},
		{Role: RoleAssistant, Content: []ContentPart{{Kind: "text", Text: "running"}}, ToolCalls: []ToolCall{
			namelessCall("bad", "", `{"a":1}`),
			namelessCall("good", "Bash", `{"command":"ls"}`),
		}},
		{Role: RoleTool, ToolCallID: "bad", Content: []ContentPart{{Kind: "text", Text: "{}"}}},
		{Role: RoleTool, ToolCallID: "good", Content: []ContentPart{{Kind: "text", Text: "a.go"}}},
	}
	out, changed := WithoutNamelessCalls(msgs)
	if !changed {
		t.Fatal("a nameless call in history must report a change")
	}
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3 (the orphan result removed): %+v", len(out), out)
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].ID != "good" {
		t.Fatalf("assistant calls = %+v", out[1].ToolCalls)
	}
	for _, m := range out {
		if m.Role == RoleTool && m.ToolCallID == "bad" {
			t.Fatal("the result answering the dropped call survived — it is now an orphan")
		}
	}
	// The surviving pair must still be adjacent-by-order for the pairing rules.
	if out[1].ToolCalls[0].ID != out[2].ToolCallID {
		t.Fatalf("the kept call/result do not pair: call=%s result=%s", out[1].ToolCalls[0].ID, out[2].ToolCallID)
	}
}

// An assistant turn that carried ONLY a nameless call and no text disappears
// entirely: keeping it would leave an empty assistant turn in the history.
func TestBP350WithoutNamelessCallsDropsEmptyAssistantTurn(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "do it"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{namelessCall("bad", "", `{}`)}},
		{Role: RoleTool, ToolCallID: "bad", Content: []ContentPart{{Kind: "text", Text: "{}"}}},
		{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "well?"}}},
	}
	out, changed := WithoutNamelessCalls(msgs)
	if !changed {
		t.Fatal("expected a change")
	}
	for _, m := range out {
		if m.Role == RoleAssistant {
			t.Fatalf("an empty assistant turn survived: %+v", m)
		}
	}
	if len(out) != 2 {
		t.Fatalf("got %d messages, want the two user turns", len(out))
	}
}

// An assistant turn with text keeps the text and loses only the call.
func TestBP350WithoutNamelessCallsKeepsText(t *testing.T) {
	msgs := []Message{{
		Role:      RoleAssistant,
		Content:   []ContentPart{{Kind: "text", Text: "I could not run it"}},
		ToolCalls: []ToolCall{namelessCall("bad", "", `{}`)},
	}}
	out, changed := WithoutNamelessCalls(msgs)
	if !changed || len(out) != 1 {
		t.Fatalf("changed=%v out=%+v", changed, out)
	}
	if len(out[0].ToolCalls) != 0 || TextOf(out[0].Content) != "I could not run it" {
		t.Fatalf("text turn was mishandled: %+v", out[0])
	}
}

// A clean history is returned untouched: an ordinary request must not be
// rewritten (that would change the prompt prefix and break every cache hit).
func TestBP350WithoutNamelessCallsCleanHistoryUntouched(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "do it"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{namelessCall("c1", "Bash", `{}`)}},
		{Role: RoleTool, ToolCallID: "c1", Content: []ContentPart{{Kind: "text", Text: "ok"}}},
	}
	out, changed := WithoutNamelessCalls(msgs)
	if changed {
		t.Fatal("a clean history must not report a change")
	}
	if len(out) != 3 {
		t.Fatalf("clean history was modified: %+v", out)
	}
}

// ArgsAsObject (BP-356) is tested here because it shares the "render what the
// model actually asked for" premise: a non-object body must not vanish.
func TestBP356ArgsAsObject(t *testing.T) {
	// A JSON object passes through with its values intact (BP-128: numbers
	// must not be re-encoded lossily — this path never re-marshals).
	obj := ArgsAsObject(`{"command":"ls","timeout":9007199254740993}`)
	if obj["command"] != "ls" {
		t.Fatalf("object lost a field: %+v", obj)
	}
	raw, _ := json.Marshal(obj["timeout"])
	if string(raw) != "9007199254740993" {
		t.Fatalf("large integer was mangled: %s", raw)
	}

	// Empty / null → empty object (a no-argument call).
	for _, s := range []string{"", "   ", "null"} {
		if got := ArgsAsObject(s); len(got) != 0 {
			t.Fatalf("ArgsAsObject(%q) = %+v, want an empty object", s, got)
		}
	}

	// A non-object value is WRAPPED, not dropped.
	if got := ArgsAsObject(`[1,2]`); got["params"] == nil {
		t.Fatalf("an array was dropped: %+v", got)
	}
	if got := ArgsAsObject(`"hello"`); got["params"] != "hello" {
		t.Fatalf("a scalar was dropped: %+v", got)
	}

	// Truncated JSON: the fragment rides as a string so the model sees what
	// the tool was asked for instead of an empty object.
	got := ArgsAsObject(`{"command":"ls -l`)
	if s, ok := got["params"].(string); !ok || s != `{"command":"ls -l` {
		t.Fatalf("a truncated body must ride as params: %+v", got)
	}
}
