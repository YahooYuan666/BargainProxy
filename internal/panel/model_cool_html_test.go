// BP-49: the account card renders the full temporary rate-limit picture per
// model — expected unblock (upstream reset) with the remaining time, and the
// gateway's own earliest retry when it precedes the reset (the hold is capped,
// so a later request probes for an early recovery).
package panel

import (
	"strings"
	"testing"
)

func TestEmbeddedPanelRendersModelRateLimitDetail(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"function modelCoolHtml(list)",
		"function fmtLeft(sec)",
		"modelCoolHtml(a.model_cooldowns)",
		"预计 ",
		" 解封",
		"网关最快 ",
		" 后重试",
		"上游重置：",
		"网关最早重试：",
		"class=\"rate-limits\"",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
	// The stale map-shaped rendering must be gone.
	if strings.Contains(html, "Object.keys(a.model_cooldowns)") {
		t.Fatal("panel still treats model_cooldowns as a map")
	}
}
