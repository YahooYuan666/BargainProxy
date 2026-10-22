package config

import "testing"

// TestClampOutputCapsAboveCeiling: a max_tokens above the model's real ceiling
// must be lowered, not forwarded. Too-large values get a 400 from some
// upstreams and a silent truncation from others; the silent case is worse
// because the client cannot tell the gateway caused it (BP-99).
func TestClampOutputCapsAboveCeiling(t *testing.T) {
	got, changed := ClampOutput(100000, "deepseek-v4.1-flash", "")
	if !changed {
		t.Fatal("100000 on a 32000-ceiling model must be clamped")
	}
	if got != 32000 {
		t.Fatalf("got %d, want 32000", got)
	}
}

// TestClampOutputLeavesLegalValues alone: a value within the ceiling must pass
// through untouched — clamping must never raise a request.
func TestClampOutputLeavesLegalValues(t *testing.T) {
	if got, changed := ClampOutput(4096, "deepseek-v4.1-flash", ""); changed || got != 4096 {
		t.Fatalf("got %d changed=%v, want 4096/false", got, changed)
	}
	if got, changed := ClampOutput(32000, "deepseek-v4.1-flash", ""); changed || got != 32000 {
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
	got, changed := ClampOutput(99999, "some-upstream-alias", "glm-5.3")
	if !changed || got != 32000 {
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
		if limit < 1024 || limit > 200000 {
			t.Errorf("implausible ceiling for %q: %d", name, limit)
		}
	}
}
