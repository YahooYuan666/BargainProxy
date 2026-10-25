package ir

import "testing"

// BP-110 reproduction: RepairToolPairing deletes a LEGITIMATE tool result
// whenever the assistant's call group and the results are separated by ANY
// other message — e.g. the assistant emits text with its call (the normal
// ZCode shape is call-only, but several drivers fold text+call into ONE
// assistant message, and parallel-call splits replay as call, text, call).
//
// The scan treats "results" as only the CONTIGUOUS run of RoleTool right
// after the call-group. A result past a text assistant (or past anything
// non-tool) is invisible to byID; the call then counts as orphaned
// (c.ID not in byID) and is DROPPED from the assistant turn, and the result
// further along is deleted outright by the "no matching call in front"
// branch. Net effect: a complete, valid call→result pair is erased from
// the history — and with it the model's memory of a tool it ran.
func TestRepairKeepsResultBeyondAnInterveningMessage(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: txt("run it")},
		{Role: RoleAssistant, Content: txt("let me run the edit")},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleAssistant, Content: txt("done")}, // intervening message
		{Role: RoleTool, ToolCallID: "c1", Content: txt("result of c1")},
		{Role: RoleUser, Content: txt("thanks")},
	}
	out, changed := RepairToolPairing(msgs)

	// The call→result pair must SURVIVE: both sides are present and match.
	foundCall, foundResult := false, false
	for _, m := range out {
		for _, c := range m.ToolCalls {
			if c.ID == "c1" {
				foundCall = true
			}
		}
		if m.Role == RoleTool && m.ToolCallID == "c1" {
			foundResult = true
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("valid call→result pair was deleted (call=%v result=%v).\nIN:\n%s\nOUT:\n%s",
			foundCall, foundResult, "(see test)", fmtMessages(out))
	}
	if !changed {
		t.Logf("note: repair saw nothing to change (acceptable if it also did no harm)")
	}
}

// The exact shape ZCode replays after a subagent turn: assistant text and the
// tool call in ONE message, result directly after — must pass through intact.
func TestRepairKeepsTextPlusCallShape(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, Content: txt("reading the file"), ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleTool, ToolCallID: "c1", Content: txt("file body")},
	}
	out, changed := RepairToolPairing(msgs)
	if changed {
		t.Fatalf("a complete pair must not be touched, but out=%s", fmtMessages(out))
	}
	if len(out) != 2 || len(out[0].ToolCalls) != 1 {
		t.Fatalf("pair damaged: %s", fmtMessages(out))
	}
}

func fmtMessages(msgs []Message) string {
	out := ""
	for i, m := range msgs {
		role := string(m.Role)
		ids := ""
		for _, c := range m.ToolCalls {
			ids += c.ID + " "
		}
		if m.ToolCallID != "" {
			ids = "result-of:" + m.ToolCallID
		}
		out += "  [" + itoa(i) + "] " + role + " " + ids + "\n"
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
