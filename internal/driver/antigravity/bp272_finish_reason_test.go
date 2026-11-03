package antigravity

import (
	"testing"

	"bargainproxy/internal/ir"
)

// BP-272 (CLIProxyAPI 24b0454811): the vendor's refusal vocabulary is wider
// than the four words the gateway used to accept. SPII (sensitive personal
// information), IMAGE_SAFETY and MALFORMED_RESPONSE all mean "the vendor
// refused the turn"; mapping them through the default branch produced
// "empty text + end_turn", which clients read as a quiet success instead of
// a refusal.
func TestMapFinishRefusalVocabulary(t *testing.T) {
	refusals := []string{
		"SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST",
		"SPII", "IMAGE_SAFETY", "MALFORMED_RESPONSE",
		// Case-insensitive: the wire has been seen lowercase too.
		"spii", "image_safety", "malformed_response",
	}
	for _, r := range refusals {
		if got := mapFinish(r); got != ir.StopContentFilter {
			t.Fatalf("mapFinish(%q) = %q, want %q", r, got, ir.StopContentFilter)
		}
	}
}

// BP-272 regression: the existing mappings must not move.
func TestMapFinishRegression(t *testing.T) {
	cases := map[string]ir.StopReason{
		"STOP":                      ir.StopEnd,
		"FINISH_REASON_UNSPECIFIED": ir.StopEnd,
		"UNKNOWN":                   ir.StopEnd,
		"TOTALLY_NEW_REASON":        ir.StopEnd, // unknown still defaults to end
		"MAX_TOKENS":                ir.StopLength,
	}
	for r, want := range cases {
		if got := mapFinish(r); got != want {
			t.Fatalf("mapFinish(%q) = %q, want %q", r, got, want)
		}
	}
	// MALFORMED_FUNCTION_CALL deliberately does NOT go through mapFinish:
	// parseSSE errorizes it before the stop mapping (BP-272 keeps that
	// demarcation — the error path is stronger than a refusal stop).
	if got := mapFinish("MALFORMED_FUNCTION_CALL"); got != ir.StopEnd {
		t.Fatalf("mapFinish(MALFORMED_FUNCTION_CALL) = %q, want %q (errorized upstream of mapFinish)", got, ir.StopEnd)
	}
}
