package trae

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// BP-125: 1005 means THIS MODEL is behind the account's plan paywall — the
// same account serves dozens of other models fine (traework2api-panel
// bb1059a/41217d3 field evidence; a 17-day account freeze came from treating
// it account-wide). Model-scoped hold with a 1h TTL now.
func Test1005IsModelScopedWithShortTTL(t *testing.T) {
	e := classifyBusiness(1005, "当前套餐不含该模型")
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("class = %s, want model_rate", e.Class)
	}
	if ttl := time.Until(e.ResetAt); ttl <= 0 || ttl > 2*time.Hour {
		t.Fatalf("ResetAt ttl = %v, want about 1h (upstream planDeniedTTL)", ttl)
	}
	if e.Code != "1005" {
		t.Fatalf("e.Code = %q, want 1005 (ladder gate + panel rely on it)", e.Code)
	}
}

// The function ladder must still walk on a 1005: the class moved, the ladder
// gate reads the code.
func Test1005StillWalksTheLadder(t *testing.T) {
	e := classifyBusiness(1005, "当前套餐不含该模型")
	if !traeWrongFunction(e) {
		t.Fatal("1005 must remain a wrong-function signal for the ladder")
	}
}

// 4008/4011 share the family treatment.
func Test4008And4011AreModelScopedToo(t *testing.T) {
	for _, code := range []int{4008, 4011} {
		e := classifyBusiness(code, "plan entitlement")
		if e.Class != uperr.ClassModelRate {
			t.Fatalf("code %d class = %s, want model_rate", code, e.Class)
		}
	}
}
