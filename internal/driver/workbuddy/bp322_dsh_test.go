package workbuddy

// BP-322：11128 裸品牌词 mop-up。
//
// 上游 11128 拒收条件是「DeepSeek Harness」出现在 system/assistant 文本的
// **任意上下文**（twa #195 三期、plugins#924 五发复现：只改身份句仍 400）。
// 我方 11128 阶梯原本只有 opener-comma 一档——comma 只治开头行。本票加
// maskDshBrand 一档：身份句改写成中性措辞、其余出现处降级为 DeepSeek。

import (
	"strings"
	"testing"
)

func TestBP322MaskDshText(t *testing.T) {
	// 身份句 → 中性措辞。
	id := "You are an AI agent powered by DeepSeek Harness."
	if got := maskDshText(id); got != "You are an AI agent powered by a local coding harness." {
		t.Fatalf("identity line = %q", got)
	}
	// 有逗号变体（#924：身份句逗号可有可无）。
	id2 := "You are an AI agent, powered by DeepSeek Harness"
	if got := maskDshText(id2); !strings.Contains(got, "local coding harness") {
		t.Fatalf("comma variant = %q", got)
	}
	// 任意上下文的裸词 → DeepSeek（#195 样本：预设描述中段）。
	bare := "本机已安装 dsh-code-review 插件（代码审查模式 agent preset）：该预设基于 DeepSeek Harness 的只读工具集构建。"
	got := maskDshText(bare)
	if strings.Contains(got, "Harness") {
		t.Fatalf("bare word survived: %q", got)
	}
	if !strings.Contains(got, "基于 DeepSeek 的只读工具集") {
		t.Fatalf("surrounding text damaged: %q", got)
	}
	// 大小写不敏感（twa：残留自检命中 "deepseek harness"）。
	if got := maskDshText("powered by deepseek harness"); strings.Contains(strings.ToLower(got), "harness") {
		t.Fatalf("case-insensitive miss: %q", got)
	}
	// 无 DSH 词的文本必须逐字节原样。
	plain := "普通的系统提示，与 DeepSeek 模型无关。"
	if maskDshText(plain) != plain {
		t.Fatalf("plain text altered: %q", maskDshText(plain))
	}
}

// TestBP322MaskDshBrandWire 只动 system/assistant；user 消息与工具文本从不
// 改动（edge 只查 system+assistant，用户文本不是拒绝面）。
func TestBP322MaskDshBrandWire(t *testing.T) {
	messages := []map[string]any{
		{"role": "system", "content": "You are an AI agent powered by DeepSeek Harness. 完成任务。"},
		{"role": "user", "content": "帮我看 DeepSeek Harness 的文档"},
		{"role": "assistant", "content": []map[string]any{{"text": "正在查 DeepSeek Harness 手册"}}},
	}
	maskDshBrand(messages)
	sys, _ := messages[0]["content"].(string)
	if strings.Contains(sys, "Harness") {
		t.Fatalf("system still carries the name: %q", sys)
	}
	user, _ := messages[1]["content"].(string)
	if !strings.Contains(user, "DeepSeek Harness") {
		t.Fatalf("user text must never be touched: %q", user)
	}
	asst := messages[2]["content"].([]map[string]any)
	if strings.Contains(asst[0]["text"].(string), "Harness") {
		t.Fatalf("assistant text still carries the name: %v", asst)
	}
}
