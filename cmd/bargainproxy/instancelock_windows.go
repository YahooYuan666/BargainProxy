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
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	processQueryLimitedInformation = 0x1000
	processQueryInformation        = 0x0400
)

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
	handle, err := openForQuery(uint32(h.pid))
	if err != nil {
		return false // no such process — stale lock
	}
	defer syscall.CloseHandle(handle)
	// BP-252 (second half): a KILLED process keeps its pid and its creation
	// time as a "zombie" object while any handle to it is still open. Windows
	// still lets OpenProcess succeed and GetProcessTimes return the original
	// values, so the identity check says "alive" for a process that has
	// already exited. Field 2026-11-01 01:34: the gateway was killed, and TWO
	// restart attempts refused ("another instance is running, pid 35052") with
	// nothing listening on the port and Get-Process reporting no such pid.
	// The exit code is the discriminator: STILL_ACTIVE means running, anything
	// else means it has exited and the lock is stale.
	if exited, code := processExited(handle); exited {
		log.Printf("instance lock: pid %d has exited (code %d); treating the lock as stale", h.pid, code)
		return false
	}
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &c, &e, &k, &u); err != nil {
		// BP-252: this used to `return true` ("cannot tell — stay
		// conservative"), which turned a coincidence into a deadlock.
		// Field 2026-11-01 01:28: the gateway was stopped, Windows handed its
		// pid to an unrelated process within seconds, OpenProcess succeeded on
		// that stranger but GetProcessTimes was refused — so the lock declared
		// a live instance forever and EVERY start exited "another instance is
		// running" with the port free. The service stayed down until the lock
		// file was deleted by hand.
		//
		// A lock that cannot be verified must not block a start: the operator
		// asked for a restart, and the pid we recorded is not provably ours.
		// Treat it as stale and say so, so a genuine double-start is still
		// visible in the log rather than silent.
		log.Printf("instance lock: cannot query pid %d (%v); treating the lock as stale", h.pid, err)
		return false
	}
	if h.creation.IsZero() {
		// Legacy pid-only lock: fall back to the old pid-alive verdict.
		return true
	}
	return time.Unix(0, c.Nanoseconds()).Equal(h.creation)
}

// stillActive is STILL_ACTIVE from GetExitCodeProcess: the only exit code that
// means "the process is still running".
const stillActive = 259

// processExited reports whether the process behind handle has already
// terminated, with its exit code. On any error it answers "not exited", so a
// process we cannot inspect keeps its lock (the conservative direction is
// correct HERE — unlike identity, where being unable to verify must not block
// a start).
func processExited(handle syscall.Handle) (bool, uint32) {
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false, 0
	}
	return code != stillActive, code
}

// openForQuery opens a pid for status queries, trying the limited-information
// right first and the older right second (BP-252). Some processes refuse
// PROCESS_QUERY_LIMITED_INFORMATION but allow PROCESS_QUERY_INFORMATION, and
// vice versa; trying both keeps the liveness answer available in more cases
// instead of falling into the "cannot tell" path.
func openForQuery(pid uint32) (syscall.Handle, error) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err == nil {
		return h, nil
	}
	if h2, err2 := syscall.OpenProcess(processQueryInformation, false, pid); err2 == nil {
		return h2, nil
	}
	return 0, err
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
