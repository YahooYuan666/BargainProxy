package autoclaw

// BP-322：autoclaw CN 网关对「DeepSeek Harness」的全文拒收（406）掩码
// （plugins#924 五发复现：只改身份句仍 406；全部掩码 200×5）。

import (
	"strings"
	"testing"
)

func TestBP322ScrubDSHIdentity(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek Harness.\n\nTask: review the repo."
	out := scrubRefusedIdentity(in)
	if strings.Contains(out, "DeepSeek Harness") {
		t.Fatalf("identity name survived: %q", out)
	}
	if !strings.Contains(out, "local coding harness") {
		t.Fatalf("identity line must be reworded to the neutral form: %q", out)
	}
	if !strings.Contains(out, "Task: review the repo.") {
		t.Fatalf("other instructions must survive: %q", out)
	}
}

func TestBP322ScrubDSHBareWord(t *testing.T) {
	in := "系统说明：dsh-code-review 预设基于 DeepSeek Harness 的只读工具集构建。"
	out := scrubRefusedIdentity(in)
	if strings.Contains(out, "Harness") {
		t.Fatalf("bare name survived: %q", out)
	}
	if !strings.Contains(out, "基于 DeepSeek 的只读工具集") {
		t.Fatalf("surrounding text damaged: %q", out)
	}
}

func TestBP322GLMCasing(t *testing.T) {
	in := "You are a coding agent powered by the glm-5.3-flash model."
	out := scrubRefusedIdentity(in)
	if !strings.Contains(out, "the GLM-5.3-flash model") {
		t.Fatalf("lowercase model name must be normalized: %q", out)
	}
}
