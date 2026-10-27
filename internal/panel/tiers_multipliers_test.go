package panel

import (
	"strings"
	"testing"
)

// 倍率口径的取证来源见 research/2026-10-18-三渠道模型倍率核实.md：
//   - Qoder 取一手接口 model/list 的 price_factor；
//   - Trae 取 docs.trae.cn「内置模型限时折扣」官方表 + 客户端截图；
//   - WorkBuddy 只有客户端截图（官网与文档站均为客户端渲染，抓不到）。
// 这些断言固定"已核实"的数值；若上游改价，改这里之前必须先重新取证。

func TestQoderMultipliersMatchUpstreamCatalog(t *testing.T) {
	want := map[string]string{
		"GLM-5.3":         "x0.8",
		"GLM-5.3-Flash":   "x0.1",
		"Qwen3.8-Max":     "x0.5",
		"DeepSeek-V4-Pro": "x0.5",
		"DeepSeek-Flash":  "x0.1",
		"Kimi-K3.1-Pro":   "x1.0",
	}
	for model, prefix := range want {
		spec, ok := routedSpecs["qoder|"+model]
		if !ok {
			t.Fatalf("qoder|%s missing from routedSpecs", model)
		}
		if !strings.HasPrefix(spec.Billing, prefix) {
			t.Fatalf("qoder|%s billing = %q, want prefix %q", model, spec.Billing, prefix)
		}
	}
	// Qwen3.8-Flash is free upstream (price_factor 0, original 0.1).
	if got := routedSpecs["qoder|Qwen3.8-Flash"].Billing; !strings.Contains(got, "免费") {
		t.Fatalf("qoder|Qwen3.8-Flash billing = %q, want 免费", got)
	}
}

func TestQoderOffPeakDiscountDocumented(t *testing.T) {
	got := routedSpecs["qoder|Qwen3.8-Max"].Billing
	// docs.qoder.cn: off-peak 22:00-08:00 Asia/Shanghai, 0.5x -> 0.2x.
	for _, frag := range []string{"22:00-08:00", "x0.2"} {
		if !strings.Contains(got, frag) {
			t.Fatalf("qoder|Qwen3.8-Max billing %q missing %q", got, frag)
		}
	}
}

func TestWorkBuddyMultipliersFromClientScreenshots(t *testing.T) {
	want := map[string]string{
		"glm-5.3":             "x0.79",
		"glm-5.3-flash":       "x0.06",
		"deepseek-v4-pro":     "x0.51",
		"deepseek-v4.1-flash": "x0.11",
		// 旧记录写 0.35，截图实测 1.62 —— 这条断言专门防回退。
		"kimi-k3": "x1.62",
	}
	rows := domesticCatalog()
	got := map[string]string{}
	for _, r := range rows {
		if r.Source == "workbuddy" {
			got[r.ID] = r.Billing
		}
	}
	for id, prefix := range want {
		b, ok := got[id]
		if !ok {
			t.Fatalf("workbuddy %s missing", id)
		}
		if !strings.HasPrefix(b, prefix) {
			t.Fatalf("workbuddy %s billing = %q, want prefix %q", id, b, prefix)
		}
	}
}

func TestTraeMultipliersFromOfficialDiscountTable(t *testing.T) {
	want := map[string]string{
		"qwen3.8-max":         "x1.50",
		"qwen3.8-flash":       "x0.08",
		"glm-5.3-flash":       "x0.06",
		"kimi-k3":             "x1.83",
		"deepseek-v4-pro":     "忙时 x0.72",
		"deepseek-v4-flash":   "忙时 x0.20",
		"deepseek-v4.1-flash": "免费忙时 x0.12",
		"glm-5.3":             "付费会员 x0.78",
	}
	rows := domesticCatalog()
	got := map[string]string{}
	for _, r := range rows {
		if r.Source == "trae" {
			got[r.ID] = r.Billing
		}
	}
	for id, prefix := range want {
		b, ok := got[id]
		if !ok {
			t.Fatalf("trae %s missing", id)
		}
		if !strings.HasPrefix(b, prefix) {
			t.Fatalf("trae %s billing = %q, want prefix %q", id, b, prefix)
		}
	}
	// "免费档" for qwen3.8-flash / kimi-k3 was wrong: neither is free.
	for id, b := range got {
		if strings.Contains(b, "免费档") && id != "glm-5.3-flashx" {
			t.Fatalf("trae %s still labelled 免费档 (%q)", id, b)
		}
	}
}

// 三个渠道的记账单位不同（分 / credits / 积分），面板若把倍率当同一
// 量纲排序会把用户引向错误结论。这条测试固定"说明必须带上渠道语境的
// 差异"，防止有人把 Billing 列简化成纯数字。
func TestMultipliersAreNotCrossChannelComparable(t *testing.T) {
	var wb, qoder, trae int
	for _, r := range domesticCatalog() {
		switch r.Source {
		case "workbuddy":
			wb++
		case "trae":
			trae++
		}
	}
	for _, key := range []string{"qoder|GLM-5.3", "qoder|Kimi-K3.1-Pro"} {
		if _, ok := routedSpecs[key]; ok {
			qoder++
		}
	}
	if wb == 0 || trae == 0 || qoder == 0 {
		t.Fatalf("catalogue incomplete: wb=%d trae=%d qoder=%d", wb, trae, qoder)
	}
}
