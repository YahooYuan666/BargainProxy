// Package clientip resolves the caller's address for logging and rate limiting
// (BP-246).
//
// THE PROBLEM: both the gateway and the panel read X-Forwarded-For FIRST,
// unconditionally. That header is caller-controlled, so any client can claim
// to be anyone. Concretely:
//
//   - the panel's login rate limiter keys on this value, so rotating a forged
//     XFF string defeats it entirely;
//   - the request log records a fabricated source IP, which is worse than no
//     IP at all when the log is what you reach for during an incident.
//
// Today the gateway binds 127.0.0.1, so the practical exposure is a local
// process that already has the machine. That is exactly the kind of "it is
// fine for now" that stops being true the day someone puts a proxy in front
// and flips the bind address — and then every log line and rate-limit bucket
// is attacker-chosen.
//
// THE FIX: XFF is honoured only when the operator says a trusted proxy sits in
// front (config `trust_forwarded_for`, default false). Otherwise the socket's
// own RemoteAddr is the answer — it cannot be forged. When trusted, the
// RIGHTMOST hop is used, not the leftmost: the leftmost is whatever the client
// typed, while the rightmost is what the last proxy in the chain observed
// (the standard "append" semantics of XFF).
package clientip

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// trustForwarded is set once at startup from the config. Atomic because the
// HTTP handlers read it from many goroutines.
var trustForwarded atomic.Bool

// SetTrustForwarded records whether a trusted reverse proxy sits in front.
// Call once at startup; it is not a per-request decision.
func SetTrustForwarded(v bool) { trustForwarded.Store(v) }

// TrustForwarded reports the current setting (tests and diagnostics).
func TrustForwarded() bool { return trustForwarded.Load() }

// From returns the caller's address.
//
// With forwarding trusted and a well-formed XFF present, the rightmost hop is
// returned. Otherwise RemoteAddr's host part is used, and when even that does
// not parse, RemoteAddr is returned verbatim rather than an empty string (an
// empty value would silently collapse every caller into one rate-limit bucket).
func From(r *http.Request) string {
	if r == nil {
		return ""
	}
	if trustForwarded.Load() {
		if h := r.Header.Get("X-Forwarded-For"); h != "" {
			parts := strings.Split(h, ",")
			// Rightmost non-empty hop: the one the nearest proxy observed.
			for i := len(parts) - 1; i >= 0; i-- {
				if hop := strings.TrimSpace(parts[i]); hop != "" {
					return hop
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
