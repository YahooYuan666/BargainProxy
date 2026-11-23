// BP-335 Form 3: Codex replays one turn as "assistant text" then "assistant
// tool_calls" as SEPARATE items. Sent upstream as two assistant turns, Kimi
// finishes the progress line and stops (misakano 9ad206bd, PR #281,
// 2026-11-21).
//
// Forms 1 and 2 both require the PREVIOUS message to carry calls, so this
// order matched neither.
package ir

import "testing"

// The headline shape: text-only assistant, then an assistant with calls.
func TestForm3TextThenCalls(t *testing.T) {
	text := Message{Role: RoleAssistant, Content: []ContentPart{{Kind: "text", Text: "我先看一下文件。"}}}
	call := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}}}
	out, changed := MergeAdjacentToolCalls([]Message{text, call})
	if !changed || len(out) != 1 {
		t.Fatalf("text-then-calls must merge: changed=%v len=%d", changed, len(out))
	}
	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("the merged turn must keep the call: %+v", out[0])
	}
	if len(out[0].Content) != 1 || out[0].Content[0].Text != "我先看一下文件。" {
		t.Fatalf("the text must ride along FIRST: %+v", out[0].Content)
	}
}

// Both sides carry text: the order must be "earlier text, then this text".
func TestForm3BothCarryText(t *testing.T) {
	text := Message{Role: RoleAssistant, Content: []ContentPart{{Kind: "text", Text: "第一段。"}}}
	call := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}},
		Content: []ContentPart{{Kind: "text", Text: "第二段。"}}}
	out, changed := MergeAdjacentToolCalls([]Message{text, call})
	if !changed || len(out) != 1 {
		t.Fatalf("changed=%v len=%d", changed, len(out))
	}
	if len(out[0].Content) != 2 || out[0].Content[0].Text != "第一段。" || out[0].Content[1].Text != "第二段。" {
		t.Fatalf("content order = %+v, want [第一段。 第二段。]", out[0].Content)
	}
}

// A text-only assistant BETWEEN two call batches is commentary about the first
// batch — folding it forward would move it. It must stay where it is.
func TestForm3DoesNotStealInterBatchCommentary(t *testing.T) {
	a := callMsg("c1")
	a.Content = []ContentPart{{Kind: "text", Text: "开头说明。"}}
	msgs := []Message{a, textMsg("中段点评。"), callMsg("c2")}
	out, changed := MergeAdjacentToolCalls(msgs)
	if changed || len(out) != 3 {
		t.Fatalf("inter-batch commentary must not merge: changed=%v len=%d", changed, len(out))
	}
}

// An empty text-only assistant is not a "text" — nothing to fold.
func TestForm3EmptyTextDoesNotMerge(t *testing.T) {
	empty := Message{Role: RoleAssistant}
	call := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}}}
	out, changed := MergeAdjacentToolCalls([]Message{empty, call})
	if changed || len(out) != 2 {
		t.Fatalf("an empty text-only assistant must not trigger Form 3: changed=%v len=%d", changed, len(out))
	}
}

// A user message in between breaks adjacency — Form 3 must not reach across.
func TestForm3RespectsUserBoundary(t *testing.T) {
	text := Message{Role: RoleAssistant, Content: []ContentPart{{Kind: "text", Text: "x"}}}
	user := Message{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "y"}}}
	call := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}}}
	out, changed := MergeAdjacentToolCalls([]Message{text, user, call})
	if changed || len(out) != 3 {
		t.Fatalf("a user turn must break the merge: changed=%v len=%d", changed, len(out))
	}
}
