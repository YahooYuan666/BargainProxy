package config

import "testing"

// TestClampOutputCapsAboveCeiling: a max_tokens above the model's real ceiling
// must be lowered, not forwarded. Too-large values get a 400 from some
// upstreams and a silent truncation from others; the silent case is worse
// because the client cannot tell the gateway caused it (BP-99).
func TestClampOutputCapsAboveCeiling(t *testing.T) {
	got, changed := ClampOutput(900000, "deepseek-v4.1-flash", "")
	if !changed {
		t.Fatal("900000 on a 393216-ceiling model must be clamped")
	}
	if got != 393216 {
		t.Fatalf("got %d, want 393216", got)
	}
}

// TestClampOutputLeavesLegalValues alone: a value within the ceiling must pass
// through untouched — clamping must never raise a request.
func TestClampOutputLeavesLegalValues(t *testing.T) {
	if got, changed := ClampOutput(4096, "deepseek-v4.1-flash", ""); changed || got != 4096 {
		t.Fatalf("got %d changed=%v, want 4096/false", got, changed)
	}
	if got, changed := ClampOutput(393216, "deepseek-v4.1-flash", ""); changed || got != 393216 {
		t.Fatalf("exact ceiling must not change: got %d changed=%v", got, changed)
	}
}

// TestClampOutputUnknownModelUntouched: an unknown model keeps its request
// rather than receiving an invented limit.
func TestClampOutputUnknownModelUntouched(t *testing.T) {
	if got, changed := ClampOutput(999999, "some-unlisted-model", ""); changed || got != 999999 {
		t.Fatalf("unknown model must be left alone: got %d changed=%v", got, changed)
	}
}

// TestClampOutputUnsetUntouched: max_tokens=0 means "unset/unlimited".
func TestClampOutputUnsetUntouched(t *testing.T) {
	if _, changed := ClampOutput(0, "deepseek-v4.1-flash", ""); changed {
		t.Fatal("unset max_tokens must not be clamped")
	}
}

// TestClampOutputFallsBackToCanonical: the driver knows the upstream id while
// the router knows the canonical name; either may carry the ceiling entry.
func TestClampOutputFallsBackToCanonical(t *testing.T) {
	// "deepseek-flash" (upstream) and "deepseek-v4.1-flash" (canonical) are both
	// in the table, so use an upstream id that is NOT, and rely on canonical.
	got, changed := ClampOutput(999999, "some-upstream-alias", "glm-5.3")
	if !changed || got != 131072 {
		t.Fatalf("canonical fallback failed: got %d changed=%v", got, changed)
	}
}

// TestFloorFromMessageVariants pins the wordings that actually appear in 400
// bodies from different vendors.
func TestFloorFromMessageVariants(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"error":{"message":"max_tokens: 1 is less than the minimum of 16","type":"invalid_request_error"}}`, 16},
		{`max_tokens must be greater than or equal to 32`, 32},
		{`max_tokens 参数必须大于等于 16`, 16},
		{`max_output_tokens: 5 is less than the minimum of 128`, 128},
		{`\u003e max_tokens must be \u003e= 64`, 64},
		{`max_tokens: must be at least 8`, 8},
		// No number → no floor.
		{`invalid api key`, 0},
		{``, 0},
		// Absurdly large numbers are rejected: real minimums are small, and a
		// big number from an unrelated message must not inflate a request.
		{`max_tokens must be at least 999999`, 0},
	}
	for _, tc := range cases {
		if got := FloorFromMessage(tc.body); got != tc.want {
			t.Errorf("FloorFromMessage(%q) = %d, want %d", tc.body, got, tc.want)
		}
	}
}

// TestOutputLimitTableSane guards against a typo that would clamp a real
// ceiling to something absurd.
func TestOutputLimitTableSane(t *testing.T) {
	for name, limit := range outputLimits {
		// Upper bound covers the largest verified ceiling (kimi-k3 1048576).
		if limit < 1024 || limit > 1048576 {
			t.Errorf("implausible ceiling for %q: %d", name, limit)
		}
	}
}

// TestOutputLimitsMatchTheTierPage pins the review's P3 requirement: the
// clamp table and the tier page must not disagree, because a mismatch means
// either the gateway silently cuts a legal budget or the page advertises a
// ceiling the gateway will never allow.
//
// Only models present in BOTH places are compared; the tier page carries
// per-source rows (trae/zcode/qoder/autoclaw) while the clamp table is keyed
// by canonical model name, so the comparison is on the canonical id.
func TestOutputLimitsMatchTheTierPage(t *testing.T) {
	// Values the panel advertises, kept here as the contract (the panel package
	// imports config, not the other way round).
	tierPage := map[string]int{
		"glm-5.3":             131072,
		"glm-5.3-flash":       131072,
		"glm-5.3-flashx":      131072,
		"glm-5.2":             131072,
		"deepseek-v4-pro":     393216,
		"deepseek-v4-flash":   393216,
		"deepseek-v4.1-flash": 393216,
		"kimi-k3":             1048576,
		"kimi-k2.7":           262144,
		"kimi-k2.6":           262144,
		"minimax-m3":          512000,
		"qwen3.8-max":         131072,
		"qwen3.8-flash":       131072,
	}
	for id, advertised := range tierPage {
		got, ok := OutputLimit(id)
		if !ok {
			t.Errorf("%s is advertised on the tier page but has no clamp entry", id)
			continue
		}
		// The clamp value must not be LOWER than what the page advertises:
		// lower means we silently cut a budget the page promised. Higher is
		// acceptable only when the clamp follows the vendor entry and the page
		// is the conservative one — but a gap either way is worth failing on
		// so the two lists are forced to be reviewed together.
		if got != advertised {
			t.Errorf("%s: clamp=%d, tier page=%d — the two must agree (review P3)", id, got, advertised)
		}
	}
}
