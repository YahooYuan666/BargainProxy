package opencodezen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BP-212: the gate recognises OpenCode by the SHAPE of its environment
// block, not merely by its presence — upstream provider.mjs says a fixed
// text only a proxy sends is itself the fingerprint. So the block must be
// OpenCode 2.2.0's own six lines, with node's platform spelling.
func TestBP212EnvironmentBlockIsOpenCodesOwnShape(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	env := zenEnvironmentFor(body)
	lines := strings.Split(env, "\n")
	want := []string{
		"Here is some useful information about the environment you are running in:",
		"<env>",
	}
	if len(lines) != 7 {
		t.Fatalf("block must be the 6-line block + nothing else, got %d lines:\n%s", len(lines), env)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d = %q, want %q", i, lines[i], w)
		}
	}
	if !strings.HasPrefix(lines[2], "  Working directory: ") {
		t.Fatalf("line 2 = %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "  Workspace root folder: ") {
		t.Fatalf("line 3 = %q", lines[3])
	}
	if lines[4] != "  Is directory a git repo: no" && lines[4] != "  Is directory a git repo: yes" {
		t.Fatalf("line 4 = %q", lines[4])
	}
	if lines[5] != "  Platform: win32" && lines[5] != "  Platform: darwin" && lines[5] != "  Platform: linux" {
		t.Fatalf("line 5 must use node's platform spelling (win32, not windows), got %q", lines[5])
	}
	if lines[6] != "</env>" {
		t.Fatalf("line 6 = %q", lines[6])
	}
	// The workspace root repeats the working directory, as OpenCode writes it.
	if strings.TrimPrefix(lines[2], "  Working directory: ") != strings.TrimPrefix(lines[3], "  Workspace root folder: ") {
		t.Fatalf("workspace root must repeat the directory:\n%s", env)
	}
}

// TestBP212GitFlagFollowsTheDirectory pins the git line: a directory holding
// .git says yes, one without says no, and an unusable path says no.
func TestBP212GitFlagFollowsTheDirectory(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := zenGitFlag(repo); got != "yes" {
		t.Fatalf("a .git directory must read yes, got %q", got)
	}
	plain := t.TempDir()
	if got := zenGitFlag(plain); got != "no" {
		t.Fatalf("no .git must read no, got %q", got)
	}
	if got := zenGitFlag(""); got != "no" {
		t.Fatalf("an unknown directory must read no, got %q", got)
	}
}

// TestBP212PlatformNeverSaysGoosWindows is the one-character fingerprint
// that would give the disguise away on Windows.
func TestBP212PlatformNeverSaysGoosWindows(t *testing.T) {
	if got := zenPlatform(); got == "windows" {
		t.Fatal("Go's GOOS spelling leaked into the block; OpenCode writes win32")
	}
}
