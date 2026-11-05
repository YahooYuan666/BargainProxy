package qoder

import (
	"testing"
)

// BP-290 (qhub #141): the INTL plan's SOTA-dedicated pool is its own credits
// bucket. The key drifted across quota endpoint generations and the value
// rides either a bare number or a {remaining,total} map; the first key
// PRESENT wins, absence means the account has no such pool (CN accounts).

func TestBP290SotaBareNumberKey(t *testing.T) {
	doc := map[string]any{
		"userQuota":   map[string]any{"remaining": float64(50), "total": float64(300), "unit": "credits", "used": float64(250)},
		"addOnQuota":  map[string]any{"remaining": float64(10), "total": float64(400)},
		"sotaCredits": float64(3800),
	}
	cr := probeCreditsFromDoc(doc).Credits
	if cr.Sota != 3800 || cr.SotaTotal != 0 {
		t.Fatalf("sota = %d/%d, want 3800/0", cr.Sota, cr.SotaTotal)
	}
	// SOTA is spendable credit the account owns (BP-290): it must ride
	// Spendable, not bench the account while plan+addon read low.
	if cr.Spendable() != 50+10+3800 {
		t.Fatalf("spendable = %d, want plan+addon+sota", cr.Spendable())
	}
}

func TestBP290SotaMapValueAndAliasKeys(t *testing.T) {
	doc := map[string]any{
		"userQuota": map[string]any{"remaining": float64(50), "total": float64(300)},
		"sotaQuota": map[string]any{"remaining": float64(100), "total": float64(3800)},
	}
	cr := probeCreditsFromDoc(doc).Credits
	if cr.Sota != 100 || cr.SotaTotal != 3800 {
		t.Fatalf("sota = %d/%d, want 100/3800", cr.Sota, cr.SotaTotal)
	}
	// First key present wins — a second alias must not override it.
	doc2 := map[string]any{"sota": float64(5), "sotaSpecial": float64(9)}
	if cr2 := probeCreditsFromDoc(doc2).Credits; cr2.Sota != 5 {
		t.Fatalf("first present key must win, sota = %d, want 5", cr2.Sota)
	}
}

func TestBP290SotaAbsentKeepsCNShape(t *testing.T) {
	doc := map[string]any{
		"userQuota":  map[string]any{"remaining": float64(50), "total": float64(300), "unit": "credits", "used": float64(250)},
		"addOnQuota": map[string]any{"remaining": float64(10), "total": float64(400)},
	}
	cr := probeCreditsFromDoc(doc).Credits
	if cr.Sota != 0 || cr.SotaTotal != 0 {
		t.Fatalf("a CN account has no SOTA pool, got %d/%d", cr.Sota, cr.SotaTotal)
	}
	if cr.Spendable() != 60 {
		t.Fatalf("spendable = %d, want plan+addon unchanged", cr.Spendable())
	}
}
