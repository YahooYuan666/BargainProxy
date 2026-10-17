// 2026-10-17 用户要求：面板品牌改成「穷鬼自建中转」，使用指南补齐
// 禁商用、每号池最多 3 个账号、纯 AI 制作声明与出境链路攻略入口。
// 这些是面向使用者的硬约束文案，回归时不能悄悄丢字。
package panel

import (
	"os"
	"strings"
	"testing"
)

func readPanelHTML(t *testing.T) string {
	t.Helper()
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	return string(raw)
}

func TestEmbeddedPanelShipsBrandAndUsageBoundaries(t *testing.T) {
	html := readPanelHTML(t)
	for _, want := range []string{
		`<div class="name">😭 穷鬼自建中转</div>`,
		`<div class="sub">力争做全网最尊重国模的中转</div>`,
		"严禁用于营业性商业目的",
		"每个号池建议最多绑 3 个账号",
		"本项目由纯 AI 制作",
		"建议直接用 agentpoints 或 AIClient2API 原项目",
		"congestionprovider=bbr2",
		"docs/跨国外网出口与BBR2调优.md",
		"onclick=\"copyNetGuide()\"",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
}

// 指南页引用的攻略文档必须真的在仓库里，避免面板指到一个不存在的文件。
func TestNetworkEgressGuideExists(t *testing.T) {
	path := "../../docs/跨国外网出口与BBR2调优.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read egress guide: %v", err)
	}
	for _, want := range []string{
		"congestionprovider=bbr2",
		"congestionprovider=default",
		"DOMAIN-SUFFIX,opencode.ai,PROXY",
		"DOMAIN-SUFFIX,googleapis.com,PROXY",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("egress guide missing %q", want)
		}
	}
}
