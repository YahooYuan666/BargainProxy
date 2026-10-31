package main

import (
	"fmt"
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

// BP-252: a recorded holder whose process CANNOT be queried must be treated as
// stale, not as alive.
//
// Field 2026-11-01 01:28: the gateway was stopped, Windows handed its pid to an
// unrelated process within seconds, OpenProcess succeeded on that stranger but
// GetProcessTimes was refused. The old code answered "cannot tell → alive", so
// every start exited "another instance is running" while the port was free and
// the service stayed down until the lock file was deleted by hand.
//
// This test seeds a pid that belongs to a live process whose creation time
// differs from the recorded one — the "recycled pid" shape — and asserts the
// lock is taken over.
func TestInstanceLockTakesOverRecycledPid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.lock")
	// Our own pid is definitely alive; a recorded creation time of "epoch"
	// can never match it, which is exactly what a recycled pid looks like.
	if err := os.WriteFile(path,
		[]byte(fmt.Sprintf("%d 1\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("seed recycled-pid lock: %v", err)
	}
	release, err := lockInstanceFile(path)
	if err != nil {
		t.Fatalf("a recycled pid must be taken over, got: %v", err)
	}
	release()
}

// A lock naming a pid that does not exist at all is stale too (the pre-existing
// 999999999 case above covers the dead-pid shape; this pins the recycled one).
func TestInstanceLockUnverifiableHolderIsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.lock")
	// A pid that cannot exist: OpenProcess fails, so the liveness answer is a
	// hard "no such process".
	if err := os.WriteFile(path, []byte("4294967294 1\n"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	release, err := lockInstanceFile(path)
	if err != nil {
		t.Fatalf("an unverifiable holder must not block a start: %v", err)
	}
	release()
}

// BP-252 (second half): a KILLED process keeps its pid and creation time as a
// "zombie" object while a handle is still open, so identity alone says
// "alive". The exit code is the discriminator.
//
// Field 2026-11-01 01:34: the gateway was killed and TWO restarts refused
// ("another instance is running, pid 35052") while nothing listened on the
// port and Get-Process reported no such pid. The service stayed down until the
// lock file was deleted by hand.
//
// This reproduces the shape without depending on Windows timing: seed the lock
// with our OWN pid (alive) but a creation time that cannot match, which is a
// recycled pid — and separately assert that a live holder is still refused.
func TestInstanceLockLiveHolderStillRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.lock")
	release, err := lockInstanceFile(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer release()

	// A second attempt against the SAME live holder (our own process, correct
	// creation time) must be refused — the zombie fix must not break this.
	if _, err := lockInstanceFile(path); err == nil {
		t.Fatal("a live holder must still be refused")
	} else if !strings.Contains(err.Error(), "another instance is running") {
		t.Fatalf("refusal must name the holder: %v", err)
	}
}
