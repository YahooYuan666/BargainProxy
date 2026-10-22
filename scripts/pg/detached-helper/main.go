// Command detached-helper starts pg_ctl with a console the user cannot see.
//
// pg_ctl starts PostgreSQL through "cmd.exe /D /C postgres.exe ...", and that
// wrapper lives exactly as long as the server does. It inherits its parent's
// console. Whatever console that is decides what the user sees:
//
//   - the launcher's console (2026-10-22): the window never closes, and closing
//     it kills the database with 0xC000013A;
//   - no console at all: pg_ctl's child is given a brand new, visible console,
//     which is the same problem with a different window.
//
// The wrapper only stays invisible when its parent already owns a console that
// has no window. CREATE_NO_WINDOW produces exactly that, but it has to be set
// on pg_ctl's own parent — pg_ctl chooses its child's flags itself and we
// cannot change them. This program is that parent.
//
// The copy launched by the script still sits on the launcher's console, so the
// first thing it does is start a second copy of itself with CREATE_NO_WINDOW
// and wait for it. Only the second copy starts pg_ctl.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// createNoWindow gives the new process a console with no visible window, which
// its children inherit instead of allocating a window of their own.
const createNoWindow = 0x08000000

func main() {
	args := os.Args[1:]
	spawn := len(args) > 0 && args[0] == "--spawn"
	if spawn {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: start-detached-helper [--spawn] <program> [args...]")
		os.Exit(2)
	}

	var flags uint32
	if !spawn {
		// Hand the real work to a copy that owns a windowless console. This
		// copy waits for it, so the script still learns pg_ctl's exit code.
		flags = createNoWindow
		args = append([]string{"--spawn"}, args...)
		args = append([]string{os.Args[0]}, args...)
	}

	argv, err := syscall.UTF16PtrFromString(commandLine(args))
	if err != nil {
		fmt.Fprintln(os.Stderr, "command line:", err)
		os.Exit(1)
	}
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	// Inherit handles only for pg_ctl, so it inherits the windowless console.
	// The outer copy must not inherit the launcher's console handles.
	err = syscall.CreateProcess(nil, argv, nil, nil, spawn, flags, nil, nil, &si, &pi)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CreateProcess:", err)
		os.Exit(1)
	}
	_, _ = syscall.WaitForSingleObject(pi.Process, syscall.INFINITE)
	var code uint32
	_ = syscall.GetExitCodeProcess(pi.Process, &code)
	syscall.CloseHandle(pi.Thread)
	syscall.CloseHandle(pi.Process)
	os.Exit(int(code))
}

// commandLine rebuilds one Windows command line from already-parsed args.
// Each argument is quoted; quotes and trailing backslashes inside it are
// escaped the way CommandLineToArgvW expects.
func commandLine(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			slashes++
		case '"':
			for j := 0; j < slashes*2+1; j++ {
				b.WriteByte('\\')
			}
			b.WriteByte('"')
			slashes = 0
		default:
			for j := 0; j < slashes; j++ {
				b.WriteByte('\\')
			}
			slashes = 0
			b.WriteByte(s[i])
		}
	}
	for j := 0; j < slashes*2; j++ {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}
