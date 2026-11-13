package streamx

import (
	"strings"
	"testing"
)

// TestSplitScannerGluedFrame BP-318：`data: {a} {b}` 在行级拆成两次 Scan，
// 其余行逐字节原样。
func TestSplitScannerGluedFrame(t *testing.T) {
	in := "data: {\"a\":1} {\"b\":2}\n\n" +
		"data: {\"c\":3}\n\n" +
		": ping\n\n" +
		"data: [DONE]\n"
	ss := NewSplitScanner(strings.NewReader(in))
	var got []string
	for ss.Scan() {
		got = append(got, ss.Text())
	}
	if err := ss.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{
		"data:{\"a\":1}",
		"data:{\"b\":2}",
		"",
		"data: {\"c\":3}",
		"",
		": ping",
		"",
		"data: [DONE]",
	}
	if len(got) != len(want) {
		t.Fatalf("scanned %d lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSplitScannerSingleObjectPassesThrough 合法单对象行必须逐字节透传
// （影响面纪律：修复只对粘帧生效）。
func TestSplitScannerSingleObjectPassesThrough(t *testing.T) {
	in := "data: {\"a\": \"brace } inside string\", \"b\": [1,2]}\r\n"
	ss := NewSplitScanner(strings.NewReader(in))
	if !ss.Scan() {
		t.Fatal("no scan")
	}
	if got, want := ss.Text(), "data: {\"a\": \"brace } inside string\", \"b\": [1,2]}"; got != want {
		t.Fatalf("line = %q, want %q (must pass through byte-identical)", got, want)
	}
	if ss.Scan() {
		t.Fatalf("unexpected extra line %q", ss.Text())
	}
}

// TestSplitConcatenatedJSON 拆分器单元：字符串里的大括号/转义不切、数组切、
// 垃圾输入原样返回、64 份封顶。
func TestSplitConcatenatedJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // 期望份数；1 = 原样
	}{
		{"两对象", `{"a":1} {"b":2}`, 2},
		{"紧贴", `{"a":1}{"b":2}`, 2},
		{"字符串里的大括号", `{"s":"} {"} {"b":2}`, 2},
		{"转义引号", `{"s":"a\"}{"} {"b":2}`, 2},
		{"数组", `[1,2] [3]`, 2},
		{"对象+数组", `{"a":1} [2]`, 2},
		{"单对象原样", `{"a":1}`, 1},
		{"非 JSON 原样", `[DONE]`, 1},
		{"垃圾原样", `hello world`, 1},
		{"括号不配平原样", `{"a":1} {"b":`, 1},
		{"原始空白保留在值外", `  {"a":1}   {"b":2}  `, 2},
	}
	for _, c := range cases {
		got := splitConcatenatedJSON(c.in)
		if len(got) != c.want {
			t.Errorf("%s: parts=%d want %d: %q", c.name, len(got), c.want, got)
			continue
		}
		if c.want == 1 && got[0] != c.in {
			t.Errorf("%s: single result must be byte-identical, got %q want %q", c.name, got[0], c.in)
		}
	}
	// 64 份封顶：65 个值 → 原样。
	many := make([]string, 65)
	for i := range many {
		many[i] = `{"n":` + string(rune('a'+i%26)) + `}`
	}
	if got := splitConcatenatedJSON(strings.Join(many, " ")); len(got) != 1 {
		t.Errorf("65 values must not split (cap), got %d parts", len(got))
	}
}

// TestToolFolderIndexFolding 常规流：同 index 的分片续同一个调用，参数按序
// 拼接，起始顺序重编号。
func TestToolFolderIndexFolding(t *testing.T) {
	f := NewToolFolder()
	r1 := f.Add(0, "call_1", "she", "")
	if !r1.Started || r1.Slot != 0 {
		t.Fatalf("first fragment must start slot 0: %+v", r1)
	}
	f.Add(0, "", "ll", "{\"cmd\":\"ls")
	f.Add(0, "", "", " -la\"}")
	r3 := f.Add(1, "call_2", "read", "{}")
	if !r3.Started || r3.Slot != 1 {
		t.Fatalf("second call must start slot 1: %+v", r3)
	}
	list := f.List()
	if len(list) != 2 {
		t.Fatalf("calls = %d, want 2", len(list))
	}
	if list[0].ID != "call_1" || list[0].Name != "shell" || list[0].Args != "{\"cmd\":\"ls -la\"}" || list[0].Slot != 0 {
		t.Fatalf("call 0 = %+v", list[0])
	}
	if list[1].ID != "call_2" || list[1].Name != "read" || list[1].Args != "{}" || list[1].Slot != 1 {
		t.Fatalf("call 1 = %+v", list[1])
	}
}

// TestToolFolderIdSplitsSameIndex BP-320 核心：同 index、新 id → 新调用
// （旧行为会把两份参数拼成 `{"a"}{"b"` 畸形 JSON）。
func TestToolFolderIdSplitsSameIndex(t *testing.T) {
	f := NewToolFolder()
	f.Add(0, "call_a", "read", "{\"p\":\"a\"}")
	// 同一个 index 上来了一个带新 id 的分片：新调用，slot 顺延。
	r := f.Add(0, "call_b", "list", "{\"d\":\"tmp\"}")
	if !r.Started || r.Slot != 1 {
		t.Fatalf("new id at a used index must open a new call: %+v", r)
	}
	// 两个调用各自的后续分片按 id 路由，互不串参。
	f.Add(0, "call_a", "", ",\"x\":1}")
	f.Add(0, "call_b", "", ",\"y\":2}")
	list := f.List()
	if len(list) != 2 {
		t.Fatalf("calls = %d, want 2", len(list))
	}
	if list[0].Args != "{\"p\":\"a\"},\"x\":1}" {
		t.Fatalf("call a args = %q, want only call_a's fragments (routing, not JSON healing)", list[0].Args)
	}
	if list[1].Args != "{\"d\":\"tmp\"},\"y\":2}" {
		t.Fatalf("call b args = %q, want no bleed from call a", list[1].Args)
	}
}

// TestToolFolderIdlessAfterSplit 无 id 分片跟随该 index 最近开启的调用
// （流式惯例：id 只在首片出现）。
func TestToolFolderIdlessAfterSplit(t *testing.T) {
	f := NewToolFolder()
	f.Add(0, "call_a", "read", "{\"p\":")
	f.Add(0, "call_b", "list", "{\"d\":")
	f.Add(0, "", "", "1}") // 无 id：归 index 最近开启的 call_b
	list := f.List()
	if list[0].Args != "{\"p\":" {
		t.Fatalf("call a args = %q, want untouched", list[0].Args)
	}
	if list[1].Args != "{\"d\":1}" {
		t.Fatalf("call b args = %q, want the id-less fragment", list[1].Args)
	}
}
