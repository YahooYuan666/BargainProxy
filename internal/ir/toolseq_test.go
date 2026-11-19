// ticket 35: back-to-back assistant tool_calls normalization (11148 defense).
package ir

import (
	"testing"
)

func txt(s string) []ContentPart { return []ContentPart{{Kind: "text", Text: s}} }
func call(id string) ToolCall    { return ToolCall{ID: id, Name: "f", Arguments: "{}"} }

// TestMergeAdjacentToolCallsForward 形态一：背靠背的 call-only assistant
// （并行调用的拆分回放）合成一条，tool_calls 依原序拼接。
func TestMergeAdjacentToolCallsForward(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: txt("hi")},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c2")}}, // 背靠背
		{Role: RoleTool, ToolCallID: "c1", Content: txt("r1")},
		{Role: RoleTool, ToolCallID: "c2", Content: txt("r2")},
	}
	out, changed := MergeAdjacentToolCalls(msgs)
	if !changed {
		t.Fatal("want change")
	}
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 (two assistants merged)", len(out))
	}
	if len(out[1].ToolCalls) != 2 || out[1].ToolCalls[0].ID != "c1" || out[1].ToolCalls[1].ID != "c2" {
		t.Fatalf("merged calls = %+v", out[1].ToolCalls)
	}
	if out[2].Role != RoleTool {
		t.Fatalf("tool results must follow the merged assistant: %+v", out[2])
	}
}

// TestMergeAdjacentToolCallsReverse 形态二：纯文本 assistant 紧跟 call-only
// assistant → 文本折入前者（两种拆分顺序都要收敛）。
func TestMergeAdjacentToolCallsReverse(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleAssistant, Content: txt("thinking out loud")}, // 反向拆分
		{Role: RoleTool, ToolCallID: "c1", Content: txt("r1")},
	}
	out, changed := MergeAdjacentToolCalls(msgs)
	if !changed || len(out) != 2 {
		t.Fatalf("changed=%v len=%d, want true/2", changed, len(out))
	}
	if len(out[0].ToolCalls) != 1 || !hasText(out[0].Content) {
		t.Fatalf("merged assistant = %+v", out[0])
	}
}

// TestMergeAdjacentToolCallsGuards 条件从严：隔消息/非 assistant 相邻都不得
// 合并（不猜语义），且对已合并结果幂等。BP-331 更新：带正文的片段现在并入
// 且正文随带（hub PR#3——Codex 把并行批拆成带文本的段，旧口径留下 11148 的
// wire 形），所以「有正文不合并」的旧断言改为断言正文随带、绝不静默丢弃。
func TestMergeAdjacentToolCallsGuards(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleAssistant, Content: txt("text"), ToolCalls: []ToolCall{call("c2")}},
	}
	out, changed := MergeAdjacentToolCalls(msgs)
	if !changed || len(out) != 1 || len(out[0].ToolCalls) != 2 {
		t.Fatalf("text-bearing fragment must merge (BP-331): changed=%v len=%d", changed, len(out))
	}
	if len(out[0].Content) != 1 || out[0].Content[0].Text != "text" {
		t.Fatalf("the fragment's text must ride along: %v", out[0].Content)
	}
	// 隔着 user 消息不合并。
	msgs = []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleUser, Content: txt("interrupt")},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c2")}},
	}
	if out, changed := MergeAdjacentToolCalls(msgs); changed || len(out) != 3 {
		t.Fatalf("non-adjacent must not merge: changed=%v len=%d", changed, len(out))
	}
	// 幂等：合并结果再跑不变。
	msgs = []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c1")}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("c2")}},
	}
	once, _ := MergeAdjacentToolCalls(msgs)
	if _, changed := MergeAdjacentToolCalls(once); changed {
		t.Fatal("merge must be idempotent")
	}
}

// --- 11148 pairing repair ---------------------------------------------------

func TestRepairToolPairingOrphans(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: txt("q1")},
		{Role: RoleAssistant, Content: txt("calling"), ToolCalls: []ToolCall{call("a"), call("b")}},
		{Role: RoleTool, ToolCallID: "a", Content: txt("ra")}, // b never answered
		{Role: RoleUser, Content: txt("q2")},
		{Role: RoleTool, ToolCallID: "ghost", Content: txt("rg")}, // stray result
	}
	out, changed := RepairToolPairing(msgs)
	if !changed {
		t.Fatal("expected repair")
	}
	if len(out) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(out), out)
	}
	if got := out[1].ToolCalls; len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("orphan call b must be dropped, got %+v", got)
	}
	if out[2].Role != RoleTool || out[2].ToolCallID != "a" {
		t.Fatalf("remaining result = %+v", out[2])
	}
	for _, m := range out {
		if m.Role == RoleTool && m.ToolCallID == "ghost" {
			t.Fatal("stray result survived")
		}
	}
}

func TestRepairToolPairingReorderAndDedupe(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("a"), call("b")}},
		{Role: RoleTool, ToolCallID: "b", Content: txt("rb")},
		{Role: RoleTool, ToolCallID: "a", Content: txt("ra")},
		{Role: RoleTool, ToolCallID: "a", Content: txt("ra-dup")},
	}
	out, changed := RepairToolPairing(msgs)
	if !changed {
		t.Fatal("expected repair")
	}
	if len(out) != 3 {
		t.Fatalf("messages = %d, want assistant + 2 results: %+v", len(out), out)
	}
	if out[1].ToolCallID != "a" || out[2].ToolCallID != "b" {
		t.Fatalf("results must follow the call order: %+v", out)
	}
	if got := out[1].Content[0].Text; got != "ra" {
		t.Fatalf("first duplicate must win, got %q", got)
	}
}

func TestRepairToolPairingDropsEmptyShell(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: txt("q")},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("a")}}, // no content, no result
		{Role: RoleUser, Content: txt("again")},
	}
	out, changed := RepairToolPairing(msgs)
	if !changed {
		t.Fatal("expected repair")
	}
	if len(out) != 2 {
		t.Fatalf("empty assistant shell must be dropped: %+v", out)
	}
	for _, m := range out {
		if m.Role == RoleAssistant {
			t.Fatalf("assistant shell survived: %+v", m)
		}
	}
}

func TestRepairToolPairingLeavesValidHistoryAlone(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: txt("q")},
		{Role: RoleAssistant, Content: txt("ok"), ToolCalls: []ToolCall{call("a")}},
		{Role: RoleTool, ToolCallID: "a", Content: txt("r")},
		{Role: RoleAssistant, Content: txt("done")},
	}
	out, changed := RepairToolPairing(msgs)
	if changed {
		t.Fatalf("valid history must not be touched: %+v", out)
	}
	if len(out) != len(msgs) {
		t.Fatalf("length changed: %d → %d", len(msgs), len(out))
	}
}
