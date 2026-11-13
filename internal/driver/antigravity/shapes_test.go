package antigravity

// BP-321：schema 六形状改写。改写层跑在 allow-list 之前，产出必须全部落在
// allow-list 词表内（type/properties/required/enum/items/nullable/description
// —— 我方端点对嵌套 anyOf 的接受度未验证，不盲信 magpie 对 Copilot 端点的
// #4374 证据）。

import (
	"strings"
	"testing"
)

func clean(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	out := cleanGeminiSchema(in)
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("cleaned schema is not a map: %#v", out)
	}
	return m
}

// TestBP321RootUnionMergesObjects 根 union：object 分支合成一个对象，
// properties 并集、required 取交集。
func TestBP321RootUnionMergesObjects(t *testing.T) {
	in := map[string]any{
		"anyOf": []any{
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path", "mode"},
			},
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"url": map[string]any{"type": "string"}},
				"required":   []any{"path", "url"},
			},
		},
	}
	out := clean(t, in)
	if out["type"] != "OBJECT" {
		t.Fatalf("type = %v, want OBJECT", out["type"])
	}
	props := out["properties"].(map[string]any)
	if _, ok := props["path"]; !ok {
		t.Fatal("merged properties lost 'path'")
	}
	if _, ok := props["url"]; !ok {
		t.Fatal("merged properties lost 'url'")
	}
	if _, ok := out["anyOf"]; ok {
		t.Fatal("union must be gone")
	}
	req := out["required"].([]any)
	if len(req) != 1 || req[0] != "path" {
		t.Fatalf("required = %v, want the intersection [path]", req)
	}
}

// TestBP321NullableSingleton null 分支折叠：唯一实质分支 + nullable。
func TestBP321NullableSingleton(t *testing.T) {
	in := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "string", "description": "the value"},
			map[string]any{"type": "null"},
		},
	}
	out := clean(t, in)
	if out["type"] != "STRING" {
		t.Fatalf("type = %v, want STRING", out["type"])
	}
	if out["nullable"] != true {
		t.Fatalf("nullable = %v, want true", out["nullable"])
	}
	if out["description"] != "the value" {
		t.Fatalf("description = %v, want kept", out["description"])
	}
}

// TestBP321EnumBranchesJoin 字符串枚举分支：成员并集。
func TestBP321EnumBranchesJoin(t *testing.T) {
	in := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string", "enum": []any{"a", "b"}},
			map[string]any{"type": "string", "enum": []any{"b", "c"}},
		},
	}
	out := clean(t, in)
	if out["type"] != "STRING" {
		t.Fatalf("type = %v, want STRING", out["type"])
	}
	enum := out["enum"].([]any)
	got := map[string]bool{}
	for _, e := range enum {
		got[e.(string)] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !got[want] {
			t.Fatalf("enum missing %q: %v", want, enum)
		}
	}
}

// TestBP321MixedUnionDegradesWithInference 混合分支：union 撤、description
// 留、type 由存活字段推断（properties→OBJECT）。
func TestBP321MixedUnionDegradesWithInference(t *testing.T) {
	in := map[string]any{
		"description": "either a patch or an URL",
		"anyOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"diff": map[string]any{"type": "string"}}},
			map[string]any{"type": "string"},
		},
	}
	out := clean(t, in)
	if out["type"] != "OBJECT" {
		t.Fatalf("type = %v, want OBJECT (inferred from merged properties)", out["type"])
	}
	if out["description"] != "either a patch or an URL" {
		t.Fatalf("description = %v, want kept", out["description"])
	}
	if _, ok := out["properties"].(map[string]any)["diff"]; !ok {
		t.Fatal("merged properties lost 'diff'")
	}
}

// TestBP321EnumOnArray 数组上的 enum：挪到 items。
func TestBP321EnumOnArray(t *testing.T) {
	in := map[string]any{
		"type": "array",
		"enum": []any{"x", "y"},
	}
	out := clean(t, in)
	if out["type"] != "ARRAY" {
		t.Fatalf("type = %v, want ARRAY", out["type"])
	}
	items := out["items"].(map[string]any)
	if items["type"] != "STRING" {
		t.Fatalf("items.type = %v, want STRING", items["type"])
	}
	if len(items["enum"].([]any)) != 2 {
		t.Fatalf("items.enum = %v, want [x y]", items["enum"])
	}
	if _, ok := out["enum"]; ok {
		t.Fatal("node-level enum must be gone")
	}
}

// TestBP321TupleCollapses prefixItems：同型 → 单 schema；异型 → 首个。
func TestBP321TupleCollapses(t *testing.T) {
	same := map[string]any{"type": "string"}
	in := map[string]any{"type": "array", "prefixItems": []any{same, map[string]any{"type": "string"}}}
	out := clean(t, in)
	items := out["items"].(map[string]any)
	if items["type"] != "STRING" {
		t.Fatalf("shared tuple → items.type = %v, want STRING", items["type"])
	}
	if _, ok := out["prefixItems"]; ok {
		t.Fatal("prefixItems must be gone")
	}
	in2 := map[string]any{"type": "array", "items": []any{
		map[string]any{"type": "string"},
		map[string]any{"type": "number"},
	}}
	out2 := clean(t, in2)
	items2 := out2["items"].(map[string]any)
	if items2["type"] != "STRING" {
		t.Fatalf("heterogeneous tuple → first position stands, got %v", items2["type"])
	}
}

// TestBP321EmptyObjectProperties OBJECT 无 properties → properties {}。
func TestBP321EmptyObjectProperties(t *testing.T) {
	out := clean(t, map[string]any{"type": "object", "description": "no args"})
	if _, ok := out["properties"].(map[string]any); !ok {
		t.Fatalf("OBJECT without properties must gain properties {}, got %#v", out)
	}
}

// TestBP321UntypedInferred 无 type 节点由身体推断（geminiTypes 同工）。
func TestBP321UntypedInferred(t *testing.T) {
	out := clean(t, map[string]any{"properties": map[string]any{"a": map[string]any{"type": "string"}}})
	if out["type"] != "OBJECT" {
		t.Fatalf("properties-only node: type = %v, want OBJECT", out["type"])
	}
	out2 := clean(t, map[string]any{"enum": []any{"p", "q"}})
	if out2["type"] != "STRING" {
		t.Fatalf("enum-only node: type = %v, want STRING", out2["type"])
	}
	out3 := clean(t, map[string]any{"items": map[string]any{"type": "string"}})
	if out3["type"] != "ARRAY" {
		t.Fatalf("items-only node: type = %v, want ARRAY", out3["type"])
	}
}

// TestBP321PlainSchemaUntouched 无六形状的 schema 必须逐键原样（影响面纪律）。
func TestBP321PlainSchemaUntouched(t *testing.T) {
	in := map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
		"required":   []any{"q"},
	}
	out := clean(t, in)
	if out["type"] != "OBJECT" || out["required"].([]any)[0] != "q" {
		t.Fatalf("plain schema altered: %#v", out)
	}
	if !strings.Contains(marshalJSON(out), "\"q\"") {
		t.Fatalf("property q lost: %#v", out)
	}
}
