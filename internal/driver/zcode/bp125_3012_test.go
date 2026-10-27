package zcode

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-125: 405 {"code":3012,"msg":"request has been blocked due to unusual
// activity."} is upstream risk control on the ACCOUNT (magpie zcode 0.1.27),
// not a payload property — the same body passes on a healthy account.
// Terminal classification keeps the flagged account out of routing instead
// of letting every request re-trip the risk rule.
func Test3012IsAccountRestricted(t *testing.T) {
	e := classifyBusiness(3012, "request has been blocked due to unusual activity.", "glm-5.3")
	if e.Class != uperr.ClassAccountRestricted {
		t.Fatalf("class = %s, want account_restricted", e.Class)
	}
}

// The quota family keeps its model-scoped shape.
func TestQuotaFamilyStaysModelScoped(t *testing.T) {
	e := classifyBusiness(1005, "quota", "glm-5.3")
	if e.Class != uperr.ClassSoftRate || e.Model == "" {
		t.Fatalf("class = %s model = %q, want soft_rate with model bound", e.Class, e.Model)
	}
}
