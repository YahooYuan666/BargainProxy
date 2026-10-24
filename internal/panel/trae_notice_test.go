package panel

import (
	"strings"
	"testing"
)

// TestTraeChannelNoticeIsRendered pins the Trae capability note on the pool
// page. The channel has two surfaces with different tool support (262144 with
// tools, 1000000 without), and that split is invisible from the UI: without the
// note an operator reads "the model ignored my tools" as a gateway bug.
//
// The numbers in the note are field measurements (2026-10-24), not marketing
// copy, so they are asserted too.
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
		"262,144",             // measured wall for tool requests
		"1,000,000",           // session surface ceiling
		"待解决",                 // the open item is stated, not hidden
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q; the Trae capability note must stay on the pool page", want)
		}
	}
}
