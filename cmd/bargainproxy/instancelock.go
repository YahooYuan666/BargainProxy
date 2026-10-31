package main

// BP-236 (Coding2API 82cf0efd): one process per DATA DIRECTORY. A second
// instance with a different port would otherwise double-write the store and
// race the pool state, and watchdog restart storms turn the bind failure
// into a log flood — the second start answers "someone is already here" and
// exits CLEANLY with code 0, which is what "already running" means to a
// supervisor (Restart=on-failure then leaves it alone).

import (
	"fmt"
	"os"
	"path/filepath"
)

// acquireInstanceLock takes the per-data-dir single-instance lock. It returns
// a release func (nil on takeover-by-death paths where none is needed) or an
// error explaining who holds it. os.Exit(0) on a held lock happens in main,
// so the message lands in the log first.
func acquireInstanceLock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("instance lock: create data dir: %w", err)
	}
	return lockInstanceFile(filepath.Join(dataDir, "instance.lock"))
}
