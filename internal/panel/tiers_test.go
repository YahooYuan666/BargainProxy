package panel

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/router"
)

func TestClassifyTiersRetiredAndNew(t *testing.T) {
	rows := []tierRow{
		{Source: "workbuddy", ID: "glm-5.3"},
		{Source: "workbuddy", ID: "glm-5.2"},
		{Source: "antigravity · Gemini", ID: "gemini-3.8-flash-high"},
	}
	active := map[string]bool{"workbuddy|glm-5.3": true, "antigravity|gemini-3.8-flash-high": true}
	added := map[string]bool{}
	got := classifyTiers(rows, active, map[string]bool{}, true, added)
	if !got[0].Routed || got[0].Retired || got[0].New {
		t.Fatalf("routed row: %+v", got[0])
	}
	if got[1].Routed || !got[1].Retired || got[1].New {
		t.Fatalf("retired row on first run must not be new: %+v", got[1])
	}
	if len(added) != 3 {
		t.Fatalf("first run must seed every key, got %d", len(added))
	}
	// Second run: a key missing from the seen baseline is new; a known retired
	// row stays retired without the new badge.
	rows2 := []tierRow{{Source: "qoder", ID: "Qwen3.8-Max"}}
	got2 := classifyTiers(rows2, map[string]bool{"qoder|Qwen3.8-Max": true}, map[string]bool{
		"workbuddy|glm-5.3": true, "workbuddy|glm-5.2": true,
	}, false, map[string]bool{})
	if !got2[0].New {
		t.Fatalf("unseen key must be new: %+v", got2[0])
	}
}

func TestMergeRoutedAddsChannelsMissingFromCatalog(t *testing.T) {
	routes := []router.Row{
		{Model: "qwen3.8-max", Channel: "qoder", UpstreamModel: "Qwen3.8-Max"},
		{Model: "kimi-k3", Channel: "workbuddy", UpstreamModel: "kimi-k3"},
	}
	channels := map[string]bool{"qoder": true, "workbuddy": true}
	rows := mergeRouted([]tierRow{{Source: "workbuddy", ID: "kimi-k3"}}, routes, channels)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (static + dynamic), got %d: %+v", len(rows), rows)
	}
	if rows[1].Source != "qoder" || rows[1].ID != "Qwen3.8-Max" {
		t.Fatalf("dynamic row = %+v", rows[1])
	}
	if rows[1].ContextLen != 1000000 || rows[1].MaxOutput != 131072 || rows[1].Billing == "未收录规格" {
		t.Fatalf("dynamic row must carry specs, got %+v", rows[1])
	}
}

func TestSortTierRowsBySourceAndModel(t *testing.T) {
	rows := []tierRow{
		{Source: "qoder", ID: "GLM-5.3"},
		{Source: "workbuddy", ID: "glm-5.3-flash"},
		{Source: "autoclaw", ID: "glm-5.3-flash"},
		{Source: "workbuddy", ID: "glm-5.3"},
		{Source: "trae", ID: "glm-5.3-flashx"},
		{Source: "qoder", ID: "Qwen3.8-Max"},
	}
	bySource := append([]tierRow(nil), rows...)
	sortTierRows(bySource, "source")
	want := []string{"workbuddy|glm-5.3", "workbuddy|glm-5.3-flash", "trae|glm-5.3-flashx", "autoclaw|glm-5.3-flash", "qoder|GLM-5.3", "qoder|Qwen3.8-Max"}
	for i, w := range want {
		if got := bySource[i].Source + "|" + bySource[i].ID; got != w {
			t.Fatalf("by source [%d] = %s, want %s", i, got, w)
		}
	}
	byModel := append([]tierRow(nil), rows...)
	sortTierRows(byModel, "model")
	// Natural order: GLM-5.3 < GLM-5.3-Flash < Qwen3.8-Max, with source as tie-break.
	wantModel := []string{"workbuddy|glm-5.3", "qoder|GLM-5.3", "workbuddy|glm-5.3-flash", "autoclaw|glm-5.3-flash", "trae|glm-5.3-flashx", "qoder|Qwen3.8-Max"}
	for i, w := range wantModel {
		if got := byModel[i].Source + "|" + byModel[i].ID; got != w {
			t.Fatalf("by model [%d] = %s, want %s", i, got, w)
		}
	}
}

func TestAttachObservedUsesRouteClientNames(t *testing.T) {
	now := time.Now()
	routes := []router.Row{
		{Model: "glm-5.3", Channel: "qoder", UpstreamModel: "GLM-5.3"},
		{Model: "deepseek-v4.1-flash", Channel: "workbuddy", UpstreamModel: "deepseek-v4.1-flash"},
	}
	rows := []tierRow{
		{Source: "qoder", ID: "GLM-5.3"},
		{Source: "workbuddy", ID: "deepseek-v4.1-flash"},
		{Source: "trae", ID: "glm-5.3-flash"},
	}
	logs := []model.RequestLog{
		{Time: now, Model: "glm-5.3", Channel: "qoder", Status: 200, Credit: 2, CreditKnown: true, InputTokens: 1000, OutputTokens: 100},
		{Time: now, Model: "glm-5.3", Channel: "qoder", Status: 200, Credit: 4, CreditKnown: true, InputTokens: 3000, OutputTokens: 300},
		{Time: now, Model: "glm-5.3", Channel: "qoder", Status: 500, Credit: 9, CreditKnown: true, InputTokens: 9000},
		{Time: now.Add(-8 * 24 * time.Hour), Model: "glm-5.3", Channel: "qoder", Status: 200, Credit: 9, CreditKnown: true, InputTokens: 9000},
		{Time: now, Model: "deepseek-v4.1-flash", Channel: "workbuddy", Status: 200, Credit: 0.5, CreditKnown: true, InputTokens: 100},
	}
	attachObserved(rows, routes, logs, now.Add(-7*24*time.Hour))
	if rows[0].ObservedN != 2 || rows[0].ObservedAvg != 3 || rows[0].ObservedMin != 2 || rows[0].ObservedMax != 4 {
		t.Fatalf("qoder observed = %+v", rows[0])
	}
	if rows[0].ObservedAvgIn != 2000 {
		t.Fatalf("qoder avg input = %d, want 2000", rows[0].ObservedAvgIn)
	}
	if rows[0].ObservedAvgOut != 200 {
		t.Fatalf("qoder avg output = %d, want 200", rows[0].ObservedAvgOut)
	}
	if rows[0].ObservedUnit != "credits" {
		t.Fatalf("qoder unit = %q", rows[0].ObservedUnit)
	}
	if rows[1].ObservedN != 1 || rows[1].ObservedAvg != 0.5 || rows[1].ObservedUnit != "分" {
		t.Fatalf("workbuddy observed = %+v", rows[1])
	}
	if rows[2].ObservedN != 0 {
		t.Fatalf("trae row must stay empty, got %+v", rows[2])
	}
}
