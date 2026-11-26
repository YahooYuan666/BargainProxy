package agybridge

// Workspace policy (BP-342), following upstream agy-acp
// (src/runtime/worker.ts ensureWorkspaceDir):
//
//	fs.mkdtempSync(path.join(os.tmpdir(), "agy-magpie-ws-"))
//
// The ACP session's `cwd` is a THROWAWAY EMPTY directory in the system temp
// tree — never the project being served, and never any directory that sits
// inside a git work tree.
//
// WHY (measured 2026-11-25): the first implementation used
// <data_dir>/agybridge/accounts/<id>/home as the session cwd. That path lives
// INSIDE the BargainProxy repository, so agy walked up, found .git, adopted the
// whole project as its workspace and started diffing it — its log filled with
//
//	failed to create patch for untracked file AutoClaw.lnk: bytes are not valid utf8
//	failed to create patch for untracked file plugins/.../local.bargainproxy-0.1.0.piplug
//
// while the user waited 9m53s for an answer that never came. The model was
// never the bottleneck: a direct ACP probe on the same account answered in
// ~13s once the workspace was empty.

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
)

// workspaceDirFor creates (once per process) a fresh empty workspace directory
// for one agy process. It deliberately lives under the OS temp root, which is
// outside every project tree, and carries an owner marker like upstream so a
// crash sweep can tell orphaned workspaces apart.
func workspaceDirFor() (string, error) {
	base := filepath.Join(os.TempDir(), "agybridge-ws-"+randToken(6))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	// A marker so a leftover directory is identifiable as ours, matching
	// upstream's `.owner-pid` convention.
	_ = os.WriteFile(filepath.Join(base, ".owner-pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	return base, nil
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

