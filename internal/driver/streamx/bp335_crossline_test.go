// BP-335: a JSON value split across several `data:` lines must be assembled.
//
// The upstream sometimes pretty-prints an event, or a proxy re-wraps one, so a
// single JSON value arrives as several `data:` lines. The per-line decode then
// failed and the driver DROPPED the frame — and a mid-stream error frame broken
// that way was invisible, so the turn ended as a silent empty success
// (CLIProxyAPI 892fc486, 2026-11-21, issue 7753).
//
// The glued-frame splitter (BP-318) handles the opposite shape: several values
// on ONE line.
package streamx

import (
	"strings"
	"testing"
)

func collect(t *testing.T, input string) []string {
	t.Helper()
	sc := NewSplitScanner(strings.NewReader(input))
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

// A pretty-printed event spanning three data lines becomes ONE complete line.
func TestAssemblesMultiLineJSON(t *testing.T) {
	input := "data: {\n" +
		`data: "error": {"code": 429,` + "\n" +
		`data: "message": "quota"}}` + "\n\n"
	got := collect(t, input)
	var dataLines []string
	for _, l := range got {
		if strings.HasPrefix(strings.TrimSpace(l), "data:") {
			dataLines = append(dataLines, l)
		}
	}
	if len(dataLines) != 1 {
		t.Fatalf("got %d data lines (%v), want 1 assembled line", len(dataLines), dataLines)
	}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(dataLines[0]), "data:"))
	if payload != `{"error": {"code": 429,"message": "quota"}}` {
		t.Fatalf("assembled payload = %q", payload)
	}
}

// A normal single-line event passes through byte-identical — the assembly must
// not touch the common case.
func TestSingleLineEventUnchanged(t *testing.T) {
	input := `data: {"a":1}` + "\n\n"
	got := collect(t, input)
	found := false
	for _, l := range got {
		if strings.TrimSpace(l) == `data: {"a":1}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("single-line event must pass through unchanged, got %v", got)
	}
}

// An unterminated assembly must NOT swallow the rest of the stream: a blank
// line is an event boundary, so the pending value is abandoned there.
func TestUnterminatedAssemblyAbandonedAtBoundary(t *testing.T) {
	input := "data: {\n" +
		"\n" +
		`data: {"ok":1}` + "\n\n"
	got := collect(t, input)
	found := false
	for _, l := range got {
		if strings.TrimSpace(l) == `data: {"ok":1}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("the event after an abandoned assembly must survive, got %v", got)
	}
}

// A structurally impossible concatenation (depth went negative) is dropped
// rather than accumulated forever.
func TestImpossibleAssemblyDropped(t *testing.T) {
	input := "data: }}}\n" +
		`data: {"ok":1}` + "\n\n"
	got := collect(t, input)
	found := false
	for _, l := range got {
		if strings.TrimSpace(l) == `data: {"ok":1}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("stream must survive an impossible fragment, got %v", got)
	}
}

// [DONE] terminates and must never be folded into an assembly.
func TestDoneNotAssembled(t *testing.T) {
	input := "data: {\n" +
		"data: [DONE]\n"
	got := collect(t, input)
	for _, l := range got {
		if strings.Contains(l, "[DONE]") {
			return
		}
	}
	t.Fatalf("a bare [DONE] must pass through, got %v", got)
}
