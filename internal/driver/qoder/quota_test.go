package qoder

import "testing"

func TestParseQuotaDetailLiveNestedShape(t *testing.T) {
	doc := map[string]any{
		"userQuota": map[string]any{
			"remaining": float64(270),
			"total":     float64(300),
			"used":      float64(30),
			"unit":      "credits",
		},
		"addOnQuota":         map[string]any{"remaining": float64(100)},
		"orgResourcePackage": map[string]any{"available": true, "remaining": float64(5)},
		"isQuotaExceeded":    false,
		"userType":           "personal_professional_trial",
		"expiresAt":          float64(1793427936797),
	}
	got := parseQuotaDetail(doc)
	if got.PlanRemain != 270 || got.PlanTotal != 300 || got.PlanUsed != 30 {
		t.Fatalf("plan = %+v", got)
	}
	if got.Unit != "credits" || got.AddonRemain != 100 || got.OrgRemain != 5 {
		t.Fatalf("extras = %+v", got)
	}
	if got.UserType != "personal_professional_trial" || got.Exceeded {
		t.Fatalf("identity = %+v", got)
	}
	if got.ExpiresAtMs != 1793427936797 {
		t.Fatalf("expires = %d", got.ExpiresAtMs)
	}
}

func TestParseQuotaDetailLegacyFlatShape(t *testing.T) {
	doc := map[string]any{
		"plan_remaining":  float64(12),
		"plan_total":      float64(20),
		"plan_used":       float64(8),
		"addon_remaining": float64(3),
		"org_remaining":   float64(1),
	}
	got := parseQuotaDetail(doc)
	if got.PlanRemain != 12 || got.PlanTotal != 20 || got.AddonRemain != 3 || got.OrgRemain != 1 {
		t.Fatalf("legacy = %+v", got)
	}
}

func TestParseQuotaDetailHidesUnavailableOrgPackage(t *testing.T) {
	doc := map[string]any{
		"userQuota":          map[string]any{"remaining": float64(1), "total": float64(2)},
		"orgResourcePackage": map[string]any{"available": false, "remaining": float64(999)},
	}
	got := parseQuotaDetail(doc)
	if got.OrgRemain != 0 {
		t.Fatalf("unavailable org package must not count: %+v", got)
	}
}
