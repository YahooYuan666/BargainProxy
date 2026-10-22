package qoder

import (
	"testing"
)

// Qoder-7b red-first test (BP-98): the quota document's
// dedicatedResourcePackages (企业专属资源包, an array of package objects)
// carries credits the old parse dropped entirely — an account with a
// dedicated package read as poorer than it is, both on the panel detail and
// in the pool balance (magpie-community/plugins packages/qoder 0.2.30
// parseUsage adds every package under "Dedicated credits").
func TestQuotaDetailIncludesDedicatedPackages(t *testing.T) {
	doc := map[string]any{
		"userQuota":  map[string]any{"remaining": float64(270), "total": float64(300), "used": float64(30), "unit": "credits"},
		"addOnQuota": map[string]any{"remaining": float64(100)},
		"dedicatedResourcePackages": []any{
			map[string]any{"remaining": float64(50), "total": float64(100), "name": "活动包"},
			map[string]any{"remaining": float64(20), "total": float64(30), "name": "专属包"},
		},
		"isQuotaExceeded": false,
		"userType":        "personal_professional_trial",
	}
	got := parseQuotaDetail(doc)
	if got.DedicatedRemain != 70 {
		t.Fatalf("DedicatedRemain = %d, want 70 (50+20 — the packages were dropped before)", got.DedicatedRemain)
	}
}

func TestProbeBalanceCountsDedicated(t *testing.T) {
	// The balance document shape (plan_used/plan_total + nested) must count
	// dedicated packages into Bonus like addon/org do.
	doc := map[string]any{
		"userQuota":  map[string]any{"remaining": float64(10), "total": float64(100), "used": float64(90), "unit": "credits"},
		"addOnQuota": map[string]any{"remaining": float64(0)},
		"dedicatedResourcePackages": []any{
			map[string]any{"remaining": float64(70)},
		},
	}
	pr := probeCreditsFromDoc(doc)
	if pr == nil {
		t.Fatal("no probe result")
	}
	if pr.Credits.Bonus < 70 {
		t.Fatalf("Bonus = %d, want >= 70 (dedicated packages count toward the balance)", pr.Credits.Bonus)
	}
}
