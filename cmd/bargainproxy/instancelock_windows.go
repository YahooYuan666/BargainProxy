//go:build windows

package main

// Windows has no flock in the stdlib and the project deliberately carries no
// golang.org/x/sys dependency, so the lock is a PID file with an EXACT
// liveness check: the holder records its pid AND its process creation time;
// a takeover requires the pid to be either dead or a DIFFERENT process.
// The creation-time comparison is what PID recycling defeats — field
// 2026-10-31 15:16: taskkill killed pid 16148, Windows handed the same pid
// to an unrelated process within the minute, and the naive pid-alive check
// refused every start ("another instance is running") with no instance
// anywhere.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const processQueryLimitedInformation = 0x1000

type lockHolder struct {
	pid      int
	creation time.Time // zero on the legacy pid-only format
}

func lockInstanceFile(path string) (func(), error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(fmt.Sprintf("%d %d\n", os.Getpid(), ownCreationTime().UnixNano()))
			return func() { f.Close() }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("instance lock: %w", err)
		}
		// The lock file exists: decide whether its owner is alive.
		holder := readHolder(path)
		if holder.pid > 0 && processStartMatches(holder) {
			return nil, fmt.Errorf("instance lock: another instance is running (pid %d, started %s)",
				holder.pid, holder.creation.Format("2006-01-02 15:04:05"))
		}
		// A dead or recycled holder: take over. The remove+recreate window is
		// raced only by another takeover candidate, and the loser lands back
		// on the exists branch.
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			return nil, fmt.Errorf("instance lock: take over stale lock: %w", rerr)
		}
	}
	return nil, fmt.Errorf("instance lock: could not settle the lock file %s", path)
}

func readHolder(path string) lockHolder {
	raw, err := os.ReadFile(path)
	if err != nil {
		return lockHolder{}
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return lockHolder{}
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return lockHolder{}
	}
	var creation time.Time
	if len(fields) > 1 {
		if nanos, perr := strconv.ParseInt(fields[1], 10, 64); perr == nil && nanos > 0 {
			creation = time.Unix(0, nanos)
		}
	}
	return lockHolder{pid: pid, creation: creation}
}

// processStartMatches answers whether pid is the SAME live process that took
// the lock: alive, and its creation time equals the recorded one. A recycled
// pid has a different start and reads as stale.
func processStartMatches(h lockHolder) bool {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(h.pid))
	if err != nil {
		return false // no such process — stale lock
	}
	defer syscall.CloseHandle(handle)
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &c, &e, &k, &u); err != nil {
		return true // cannot tell — stay conservative
	}
	if h.creation.IsZero() {
		// Legacy pid-only lock: fall back to the old pid-alive verdict.
		return true
	}
	return time.Unix(0, c.Nanoseconds()).Equal(h.creation)
}

// ownCreationTime is this process's creation time, recorded with the lock.
func ownCreationTime() time.Time {
	var c, e, k, u syscall.Filetime
	self, serr := syscall.GetCurrentProcess()
	if serr != nil {
		return time.Time{}
	}
	if err := syscall.GetProcessTimes(self, &c, &e, &k, &u); err != nil {
		return time.Time{}
	}
	return time.Unix(0, c.Nanoseconds())
}
