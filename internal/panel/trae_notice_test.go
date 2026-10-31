package panel

import (
	"strings"
	"testing"
)

// TestTraeChannelNoticeIsRendered pins the Trae capability note on the pool
// page. The channel has two surfaces with different tool support (1024000 with
// tools, 1000000 without), and that split is invisible from the UI: without the
// note an operator reads "the model ignored my tools" as a gateway bug.
//
// The numbers in the note are field measurements (2026-11-01 re-measurement),
// not marketing copy, so they are asserted too.
func TestTraeChannelNoticeIsRendered(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(raw)

	for _, want := range []string{
		`id="chanNotice"`,     // the container the renderer targets
		"renderChannelNotice", // wired into renderAccounts
		"llm_utils_chat",      // the tool-capable surface
		"chat_sessions",       // the long-context surface
		"1,024,000",           // measured wall for tool requests (BP-239/BP-260)
		"1,000,000",           // session surface ceiling
		"4022",                // measured error code beyond 1.02M
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q; the Trae capability note must stay on the pool page", want)
		}
	}
}
