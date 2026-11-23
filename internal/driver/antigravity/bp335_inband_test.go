// BP-335: a 200 response whose body is a plain JSON error document — not an
// SSE stream — used to be skipped line by line as "not a data: frame", so the
// quota/refusal inside it never reached the classifier and the turn ended as a
// silent empty success (CLIProxyAPI ec25cc83, 2026-11-22).
//
// The driver requests `alt=sse` explicitly (antigravity.go:325), so a normal
// answer is always an SSE stream; a bare JSON body is therefore an error.
package antigravity

import (
	"testing"

	"bargainproxy/internal/uperr"
)

func TestBareJSONErrorBodyIsClassified(t *testing.T) {
	// A 200 whose body is a JSON error document with no candidates.
	payload := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Individual quota reached"}}`
	e := detectFrameError(payload, "gemini-3.8-flash-high")
	if e == nil {
		t.Fatal("a bare JSON error body must be classified, not skipped")
	}
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate", e.Class)
	}
}

// A bare JSON body with NO error key is not an error — the driver must keep
// tolerating it (it becomes an empty reply, which the gateway's net catches).
func TestBareJSONWithoutErrorIsNotClassified(t *testing.T) {
	payload := `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`
	if e := detectFrameError(payload, "m"); e != nil {
		t.Fatalf("a body without an error key must not be classified as one: %+v", e)
	}
}
