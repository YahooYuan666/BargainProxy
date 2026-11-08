// drain.go — read a finished SSE body to its end so the pooled connection
// survives (BP-298, CLIProxyAPI ce38c75).
//
// A read loop that breaks on the terminal frame ([DONE] / message_stop)
// leaves the response body unread: net/http cannot return the connection to
// the pool and every request pays a fresh TCP+TLS handshake (through a
// proxy, several round trips). Draining what is left — bounded, and only on
// clean termination — lets the transport reuse the connection.
package httpx

import "io"

// drainCap bounds the drain: after a terminal frame the remainder is the
// chunked terminator at most; 64 KiB is generous headroom for trailers.
const drainCap = 64 << 10

// Drain reads up to drainCap bytes from r, stopping at EOF. Call it only
// after a clean terminal frame — an error path's body state is unknowable
// and the connection is going away anyway.
func Drain(r io.Reader) {
	if r == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r, drainCap))
}
