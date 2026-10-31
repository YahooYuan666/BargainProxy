// Package safego runs background work so that a panic in ONE job cannot take
// the whole gateway down (BP-245).
//
// WHY THIS EXISTS: net/http already recovers panics inside a request handler,
// so a malformed request cannot kill the process. Everything the gateway runs
// OUTSIDE that protection — the task scheduler's minute tick, the panels'
// background flow watchers, the zen sync loop — had no recover at all. One
// nil-pointer in a checkin parser would therefore end the process, and the
// process serves the LLM traffic of the very session debugging it.
//
// The upstream lesson is the same one magpie recorded in #4553 (a stray byte in
// a request panicked their local tokenizer): a background job is not worth the
// process. They fixed it by catching the panic and failing only that request.
//
// Design notes:
//   - recover does NOT swallow the problem: the stack is logged so the panic
//     is diagnosable, not silently buried;
//   - a panicking job is abandoned, its siblings and the caller continue;
//   - no retry here. Whether to retry is the caller's policy.
package safego

import (
	"log"
	"runtime/debug"
)

// Go runs fn in a new goroutine, recovering any panic.
//
//	name is a short label that appears in the log line, so "which job died" is
//	answerable from the log alone.
func Go(name string, fn func()) {
	go func() {
		defer Recover(name)
		fn()
	}()
}

// Recover is the deferred half, for callers that already own their goroutine
// body and only want the guard. Use it as:
//
//	go func() {
//	    defer safego.Recover("watchWBFlow")
//	    ...
//	}()
func Recover(name string) {
	if r := recover(); r != nil {
		log.Printf("panic in background job %q: %v\n%s", name, r, debug.Stack())
	}
}
