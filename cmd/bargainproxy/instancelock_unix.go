//go:build !windows

package main

// The Unix lock is an advisory flock: the kernel releases it when the holder
// dies, so there is no stale-lock problem to take over.

import (
	"fmt"
	"os"
	"syscall"
)

func lockInstanceFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("instance lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		pid, _ := readPID(path)
		if pid > 0 {
			return nil, fmt.Errorf("instance lock: another instance is running (pid %d)", pid)
		}
		return nil, fmt.Errorf("instance lock: another instance is running")
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return func() { f.Close() }, nil
}

func readPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscanf(string(raw), "%d", &pid)
	return pid, err
}
