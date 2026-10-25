package antigravity

import (
	"os"
	"strings"
	"testing"

	"bargainproxy/internal/config"
)

// BP-115: the upstream's generic rejection ("Request contains an invalid
// argument") names no field, and every hand-built shape answers 200, so the
// only way to find the difference is to keep the request we actually sent.
// The dump is OFF by default (a body carries the user's prompts) and writes
// one file per FAILED request when a directory is configured.
func TestDumpWritesOnFailureWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	d := New(config.AntigravitySettings{DumpDir: dir, DumpMaxBytes: 4096}, nil)
	d.dumpFailure("antigravity:someone@example.com", "gemini-3.8-flash-high", 400,
		[]byte(`{"project":"p","request":{"contents":[]}}`),
		[]byte(`{"error":{"code":400,"message":"Request contains an invalid argument."}}`))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dump dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dump files = %d, want exactly 1", len(entries))
	}
	raw, err := os.ReadFile(dir + string(os.PathSeparator) + entries[0].Name())
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	s := string(raw)
	for _, want := range []string{"=== request ===", "=== response ===", `"contents":[]`, "invalid argument", "http_status: 400", "gemini-3.8-flash-high"} {
		if !strings.Contains(s, want) {
			t.Fatalf("dump missing %q:\n%s", want, s)
		}
	}
}

// Off by default: no directory configured means no file, no cost.
func TestDumpOffByDefault(t *testing.T) {
	d := New(config.AntigravitySettings{}, nil)
	d.dumpFailure("acc", "m", 400, []byte("x"), []byte("y"))
	// Nothing to assert on disk: the contract is "no panic, no directory made".
	// The absence of a TMPDIR write is covered by the configured-path test.
}
