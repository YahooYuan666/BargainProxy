// BP-355 (qoder2api-hub 71b9f40f / 732fb0db): the Anthropic surface has no
// place for the reasoning subset, and clients read EITHER extension name — so
// both go out, with the same number.
package anthropic

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

func TestBP355ThinkingTokensBothNames(t *testing.T) {
	u := UsageFromIR(ir.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 30})
	if u.OutputTokensDetails == nil {
		t.Fatal("reasoning tokens did not reach output_tokens_details")
	}
	if u.OutputTokensDetails.ThinkingTokens != 30 {
		t.Fatalf("thinking_tokens = %d, want 30", u.OutputTokensDetails.ThinkingTokens)
	}
	if u.OutputTokensDetails.ReasoningTokens != 30 {
		t.Fatalf("reasoning_tokens = %d, want 30 (both names carry the same value)", u.OutputTokensDetails.ReasoningTokens)
	}
}

// No reasoning reported → the whole object stays absent. An explicit 0 would
// claim a measurement the upstream never made (the same rule the cache fields
// follow, BP-53).
func TestBP355NoReasoningOmitsDetails(t *testing.T) {
	u := UsageFromIR(ir.Usage{InputTokens: 10, OutputTokens: 5})
	if u.OutputTokensDetails != nil {
		t.Fatalf("output_tokens_details present with no reasoning: %+v", u.OutputTokensDetails)
	}
	b, _ := json.Marshal(u)
	if got := string(b); got != `{"input_tokens":10,"output_tokens":5}` {
		t.Fatalf("wire shape = %s", got)
	}
}

// The wire names are the contract.
func TestBP355WireFieldNames(t *testing.T) {
	u := UsageFromIR(ir.Usage{InputTokens: 1, OutputTokens: 2, ReasoningTokens: 3})
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	d, ok := doc["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("no output_tokens_details in %s", b)
	}
	if d["thinking_tokens"] == nil || d["reasoning_tokens"] == nil {
		t.Fatalf("a client reading either name finds nothing: %s", b)
	}
}
