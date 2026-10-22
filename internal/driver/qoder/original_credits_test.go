package qoder

import "testing"

// Qoder-7c red-first test (BP-98): the discount price the upstream reports
// alongside the charged credits (original_credits — the struck-through
// price while a promotion runs) must reach the IR usage so the log row can
// show what the request would have cost undiscounted. The driver parsed it
// and then dropped it at the ir.Usage boundary.
func TestOriginalCreditsReachesIR(t *testing.T) {
	u := irUsageFromUsageObj(&usage{
		PromptTokens:     3,
		CompletionTokens: 2,
		Credits:          0.2,
		OriginalCredits:  0.5,
		Billable:         true,
	})
	if !u.CreditOriginalKnown {
		t.Fatal("original_credits must reach ir.Usage (the discount price was parsed then dropped)")
	}
	if u.CreditOriginal != 0.5 {
		t.Fatalf("CreditOriginal = %v, want 0.5", u.CreditOriginal)
	}
	// No original reported: the flag stays down (a zero strike-through is
	// "no data", not "free").
	u2 := irUsageFromUsageObj(&usage{Credits: 1.0, Billable: true})
	if u2.CreditOriginalKnown {
		t.Fatal("absent original_credits must not read as known")
	}
}
