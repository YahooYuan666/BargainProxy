package workbuddy

import (
	"strings"
	"testing"
)

// BP-336 (2026-11-23): the CN and global /v3/config catalogues both name the
// Hy4 family as `hy4std` (flagship), `hy4flash-free` (limited-time free tier)
// and `hy4flash` (paid tier). `hy4free` is no longer a catalogue id — it only
// survives as an alias of `hy4std` on the CN side and is absent on global —
// so advertising it would send global accounts to 11102 "service info not
// found". Verified with a zero-cost GET /v3/config on both realms.
func TestStaticModelsIncludeHy4FlashFamily(t *testing.T) {
	models, err := (&Driver{}).Models(nil)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	joined := "," + strings.Join(models, ",") + ","
	for _, want := range []string{"hy4std", "hy4flash", "hy4flash-free"} {
		if !strings.Contains(joined, ","+want+",") {
			t.Errorf("%q missing from the WorkBuddy model list: %v", want, models)
		}
	}
	if strings.Contains(joined, ",hy4free,") {
		t.Errorf("hy4free is not a catalogue id anymore (CN alias of hy4std, absent on global): %v", models)
	}
	// The paid and free ids differ by a suffix — a prefix-only match would
	// let one stand in for the other, so pin both spellings exactly.
	if !strings.Contains(joined, ",hy4flash-free,") || !strings.Contains(joined, ",hy4flash,") {
		t.Errorf("hy4flash and hy4flash-free must both be listed verbatim: %v", models)
	}
}
