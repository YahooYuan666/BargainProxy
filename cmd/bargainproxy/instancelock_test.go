package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BP-236: the second holder of the same lock file is refused, and a lock
// whose recorded pid is dead is taken over (Windows semantics; the flock
// build releases on death by itself).
func TestInstanceLockRejectsSecondHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.lock")
	release, err := lockInstanceFile(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer release()

	_, err = lockInstanceFile(path)
	if err == nil {
		t.Fatal("second lock must be refused")
	}
	if !strings.Contains(err.Error(), "another instance is running") {
		t.Fatalf("refusal must name the holder: %v", err)
	}
}

func TestInstanceLockTakesOverStaleLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.lock")
	// A pid that cannot exist on any platform.
	if err := os.WriteFile(path, []byte("999999999\n"), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}
	release, err := lockInstanceFile(path)
	if err != nil {
		t.Fatalf("stale lock must be taken over: %v", err)
	}
	defer release()

	// The taken-over lock now records OUR pid and refuses others.
	_, err = lockInstanceFile(path)
	if err == nil {
		t.Fatal("post-takeover lock must refuse a second holder")
	}
}
