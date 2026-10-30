package gateway

import (
	"testing"
	"time"
)

// BP-219: fbWaitFor returned the UNCAPPED base when the request estimates to
// zero tokens — bypassing channel_first_byte_windows entirely. Field
// 2026-10-31: with workbuddy capped at 10s, the open-design memory legs ran
// 10.278s with no window expiry and no cascade (the client's own 10s timeout
// killed them first), while a 300ms live probe proved the whole window
// mechanism works. The only window value consistent with the logs is the
// uncapped 30s base: estimate=0 skipped the cap. A zero-estimate request is
// exactly the one we understand LEAST — it must get the tightest window, not
// the loosest.
func TestBP219ZeroEstimateHonoursChannelCeiling(t *testing.T) {
	if got := fbWaitFor(30*time.Second, reqWithText(0), 10*time.Second); got != 10*time.Second {
		t.Fatalf("zero-estimate request: window = %s, want the 10s channel ceiling (the uncapped base defeats per-channel caps)", got)
	}
	if got := fbWaitFor(30*time.Second, reqWithText(0), fbWindowCeiling); got != 30*time.Second {
		t.Fatalf("zero-estimate request under the global ceiling: got %s, want the base", got)
	}
	// Requests with text keep the old behaviour: base when it fits.
	if got := fbWaitFor(8*time.Second, reqWithText(6), fbWindowCeiling); got != 8*time.Second {
		t.Fatalf("small request under the ceiling: got %s, want the base", got)
	}
}
