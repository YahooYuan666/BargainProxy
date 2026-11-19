package ir

// BP-331 (hub niyouqianma PR#3, 2c497cba): Codex replays one parallel tool
// batch as SEVERAL assistant fragments, and the later fragments may carry
// text (reasoning preamble + calls, then comment + calls). Form 1 must merge
// those text-bearing fragments instead of leaving the split batch — the wire
// shape the DeepSeek-family upstream answers 11148 to. Nothing is dropped:
// the fragment's text rides along.

import "testing"

func callMsg(id string) Message {
	return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: "shell", Arguments: "{}"}}}
}

func textMsg(s string) Message {
	return Message{Role: RoleAssistant, Content: []ContentPart{{Kind: "text", Text: s}}}
}

func TestBP331Form1CarriesText(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "q"}}},
		callMsg("c1"),                    // call-only fragment
		textMsg("先看目录结构"), callMsg("c2"), // text-bearing fragment of the same batch
		{Role: RoleTool, ToolCallID: "c1", Content: []ContentPart{{Kind: "text", Text: "r1"}}},
		{Role: RoleTool, ToolCallID: "c2", Content: []ContentPart{{Kind: "text", Text: "r2"}}},
	}
	out, changed := MergeAdjacentToolCalls(msgs)
	if !changed {
		t.Fatal("the split batch must merge")
	}
	if len(out) != 4 {
		t.Fatalf("messages = %d, want 4 (user, merged assistant, two results)", len(out))
	}
	m := out[1]
	if len(m.ToolCalls) != 2 {
		t.Fatalf("merged tool_calls = %d, want 2", len(m.ToolCalls))
	}
	if len(m.Content) != 1 || m.Content[0].Text != "先看目录结构" {
		t.Fatalf("the fragment's text must ride along: %v", m.Content)
	}
	if out[2].ToolCallID != "c1" || out[3].ToolCallID != "c2" {
		t.Fatal("results must stay after the merged turn")
	}
}

// The common call-only shape still merges exactly as before.
func TestBP331Form1EmptyContentRegression(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "q"}}},
		callMsg("c1"), callMsg("c2"),
	}
	out, changed := MergeAdjacentToolCalls(msgs)
	if !changed || len(out) != 2 || len(out[1].ToolCalls) != 2 {
		t.Fatalf("call-only merge regression: changed=%v len=%d", changed, len(out))
	}
	if len(out[1].Content) != 0 {
		t.Fatal("empty fragments add no content")
	}
}

// Form 2 (text-only fragment folding into a call-only assistant) unchanged.
func TestBP331Form2Regression(t *testing.T) {
	msgs := []Message{
		callMsg("c1"),
		textMsg("我来跑一下"),
	}
	out, changed := MergeAdjacentToolCalls(msgs)
	if !changed || len(out) != 1 || len(out[0].ToolCalls) != 1 {
		t.Fatalf("form 2 regression: changed=%v len=%d", changed, len(out))
	}
	if len(out[0].Content) != 1 || out[0].Content[0].Text != "我来跑一下" {
		t.Fatalf("form 2 text fold: %v", out[0].Content)
	}
}

// Two ADJACENT text+calls fragments (both carry text) merge — each fragment's
// text is kept, never dropped.
func TestBP331Form1BothCarryText(t *testing.T) {
	a := callMsg("c1")
	a.Content = []ContentPart{{Kind: "text", Text: "开头说明。"}}
	b := callMsg("c2")
	b.Content = []ContentPart{{Kind: "text", Text: "第二段思路。"}}
	out, changed := MergeAdjacentToolCalls([]Message{a, b})
	if !changed || len(out) != 1 {
		t.Fatalf("both-text merge: changed=%v len=%d", changed, len(out))
	}
	if len(out[0].ToolCalls) != 2 || len(out[0].Content) != 2 {
		t.Fatalf("content=%d calls=%d, want 2/2 (both fragments' text kept)", len(out[0].Content), len(out[0].ToolCalls))
	}
}

// A STANDALONE text-only assistant between two call fragments does NOT chain
// the merge (the middle message has no calls) — the gate stays at ADJACENT
// fragments only.
func TestBP331StandaloneTextBreaksChain(t *testing.T) {
	a := callMsg("c1")
	a.Content = []ContentPart{{Kind: "text", Text: "开头说明。"}}
	msgs := []Message{a, textMsg("中段点评。"), callMsg("c2")}
	out, changed := MergeAdjacentToolCalls(msgs)
	if changed || len(out) != 3 {
		t.Fatalf("non-adjacent fragments must not merge: changed=%v len=%d", changed, len(out))
	}
}
