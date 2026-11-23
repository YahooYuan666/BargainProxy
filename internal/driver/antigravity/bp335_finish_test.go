// BP-335: the vendor's remaining refusal finishReasons must not read as clean
// finishes. Falling to StopEnd kept sticky, retried nothing, and handed the
// client a short answer with no hint of the refusal (CLIProxyAPI 2c8b32fd,
// 2026-11-21).
package antigravity

import (
	"testing"

	"bargainproxy/internal/ir"
)

func TestRemainingRefusalFinishReasons(t *testing.T) {
	for _, reason := range []string{
		"UNEXPECTED_TOOL_CALL", "LANGUAGE", "NO_IMAGE", "OTHER",
	} {
		if got := mapFinish(reason); got != ir.StopContentFilter {
			t.Fatalf("finishReason %s = %q, want content_filter", reason, got)
		}
	}
}

// The BP-272 set must keep working.
func TestBP272RefusalSetStillMaps(t *testing.T) {
	for _, reason := range []string{"SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "IMAGE_SAFETY", "MALFORMED_RESPONSE"} {
		if got := mapFinish(reason); got != ir.StopContentFilter {
			t.Fatalf("finishReason %s = %q, want content_filter", reason, got)
		}
	}
}

// A normal finish is still a normal finish.
func TestNormalFinishReasonsUnchanged(t *testing.T) {
	if got := mapFinish("STOP"); got != ir.StopEnd {
		t.Fatalf("STOP = %q, want stop_end", got)
	}
	if got := mapFinish("MAX_TOKENS"); got != ir.StopLength {
		t.Fatalf("MAX_TOKENS = %q, want length", got)
	}
}
