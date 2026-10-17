// BP-50/BP-52 panel contract: the request-log table drops the User-Agent
// column (still recorded server-side) and stops wrapping cells so the long
// table scrolls horizontally instead of stacking four-line model names; the
// token chart is the real-time-axis SVG renderer (agentpoints parity).
package panel

import (
	"strings"
	"testing"
)

func TestEmbeddedPanelLogTableDropsUserAgentAndStopsWrapping(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"#logs th,#logs td{white-space:nowrap}",
		`<td colspan="16" class="muted">无匹配记录</td>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
	// UA stays searchable but is no longer a rendered column.
	if !strings.Contains(html, "l.user_agent") {
		t.Fatal("UA must stay in the search haystack")
	}
	for _, gone := range []string{"<th>User-Agent</th>", "esc(l.user_agent||'—')"} {
		if strings.Contains(html, gone) {
			t.Fatalf("embedded panel still renders the UA column (%q)", gone)
		}
	}
}

// BP-53: cache-hit-rate parity with agentpoints — the shared rate helpers, the
// KPI card, the detail column and the per-request log column.
func TestEmbeddedPanelShipsCacheHitRate(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"function cacheRateText(hit, knownPrompt)",
		"function cacheRateCell(hit, knownPrompt)",
		"缓存命中率",
		"cacheRateText(totals.cachedHit, totals.cachedPrompt)",
		"r.cache_known ? cacheRateCell(r.cached_tokens, r.cache_known_tokens)",
		"l.cache_known ? cacheRateCell(l.cached_tokens, l.input_tokens)",
		"cache_known_tokens",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
}

func TestEmbeddedPanelShipsRealtimeUsageChart(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"function parsePointTime(p)",
		"function fmtTokTimeLabel(p, withDate)",
		"renderUsageChart(d.series||[])",
		"preserveAspectRatio=\"xMidYMid meet\"",
		"usGradP",
		"class=\"usbar\"",
		"#usChart .gl{stroke:var(--line-soft)",
		"#usChart .us-empty",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
	// The old flexbar renderer must be gone.
	if strings.Contains(html, "height:86px;width:22px") {
		t.Fatal("old index-spaced bar renderer still present")
	}
}
