package opencodezen

import (
	"os"
	"strings"
	"testing"
)

// BP-188: since 2026-10-29 the upstream fingerprints the literal "Working
// directory: unknown" as a proxy constant and 429s it (magpie-community/
// plugins 1f56706) — the injected env block must carry a real directory.
func TestEnvBlockNeverSaysUnknown(t *testing.T) {
	// A request whose texts carry no directory falls back to the process
	// working directory — never the refused literal.
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	env := zenEnvironmentFor(body)
	if strings.Contains(env, "unknown") {
		t.Fatalf("the fallback block still says unknown: %q", env)
	}
	if wd, err := os.Getwd(); err == nil && !strings.Contains(env, wd) {
		t.Fatalf("fallback should be the process working directory %q: %q", wd, env)
	}
	// BP-212: the block is now OpenCode 2.2.0's own six-line shape — a
	// shortened block is itself a fingerprint (upstream provider.mjs).
	if !strings.HasPrefix(env, "Here is some useful information about the environment you are running in:\n<env>\n  Working directory: ") {
		t.Fatalf("env block shape changed: %q", env)
	}
	for _, want := range []string{"\n  Workspace root folder: ", "\n  Is directory a git repo: ", "\n  Platform: ", "\n</env>"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env block missing %q: %q", want, env)
		}
	}
	if strings.HasSuffix(env, "\n</env>\n") {
		t.Fatalf("the block must end at </env>: %q", env)
	}
}

// TestEnvBlockUsesTheRequestsOwnDirectory pins the extraction: OpenCode's
// own `<cwd>…</cwd>` wins, a "Working directory: …" line is the second
// source, and the literal "unknown" on such a line is skipped.
func TestEnvBlockUsesTheRequestsOwnDirectory(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "You are a coding agent."},
		map[string]any{"role": "user", "content": "<env>\n  Working directory: /home/dev/proj\n  <cwd>/home/dev/proj</cwd>\n</env>\nfix the bug"},
	}}
	env := zenEnvironmentFor(body)
	if !strings.Contains(env, "/home/dev/proj") {
		t.Fatalf("the request's own directory must ride the block: %q", env)
	}
	if strings.Contains(env, "<cwd>") {
		t.Fatalf("the raw cwd tag must not leak: %q", env)
	}

	lineOnly := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Working directory: D:\\Projects\\demo"},
	}}
	if env := zenEnvironmentFor(lineOnly); !strings.Contains(env, `D:\Projects\demo`) {
		t.Fatalf("a Working-directory line must be picked up: %q", env)
	}

	unknownLine := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Working directory: unknown"},
	}}
	if env := zenEnvironmentFor(unknownLine); strings.Contains(env, "unknown") {
		t.Fatalf("a client-carried unknown must be skipped: %q", env)
	}
}
