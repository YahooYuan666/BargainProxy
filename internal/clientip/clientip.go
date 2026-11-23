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

// trustedProxies is the optional peer allowlist (BP-335, Coding2API 21879bdd).
// Empty means "trust every peer" — the old behaviour, kept for operators whose
// proxy address is not fixed. When set, XFF is honoured ONLY if the socket peer
// (RemoteAddr) is in the list: otherwise any caller that can reach the port
// forges XFF and defeats the login rate limiter.
var trustedProxies atomic.Pointer[[]*net.IPNet]

// SetTrustForwarded records whether a trusted reverse proxy sits in front.
// Call once at startup; it is not a per-request decision.
func SetTrustForwarded(v bool) { trustForwarded.Store(v) }

// TrustForwarded reports the current setting (tests and diagnostics).
func TrustForwarded() bool { return trustForwarded.Load() }

// SetTrustedProxies installs the peer allowlist (IPs or CIDRs). An empty list
// clears it, restoring "any peer may set XFF". A malformed entry is an error
// so startup can refuse rather than silently trusting nobody.
func SetTrustedProxies(entries []string) error {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return &net.ParseError{Type: "trusted proxy address", Text: e}
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, ipnet, err := net.ParseCIDR(e)
		if err != nil {
			return err
		}
		nets = append(nets, ipnet)
	}
	if len(nets) == 0 {
		trustedProxies.Store(nil)
		return nil
	}
	trustedProxies.Store(&nets)
	return nil
}

// TrustedProxies reports the configured allowlist length (tests/diagnostics).
func TrustedProxies() int {
	if p := trustedProxies.Load(); p != nil {
		return len(*p)
	}
	return 0
}

// peerTrusted reports whether XFF may be honoured for this request: forwarding
// must be enabled AND (no allowlist OR the socket peer is in it).
func peerTrusted(r *http.Request) bool {
	if !trustForwarded.Load() {
		return false
	}
	list := trustedProxies.Load()
	if list == nil || len(*list) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range *list {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// From returns the caller's address.
//
// With forwarding trusted (and, when an allowlist is configured, the peer being
// on it) and a well-formed XFF present, the rightmost hop is returned.
// Otherwise RemoteAddr's host part is used, and when even that does not parse,
// RemoteAddr is returned verbatim rather than an empty string (an empty value
// would silently collapse every caller into one rate-limit bucket).
func From(r *http.Request) string {
	if r == nil {
		return ""
	}
	if peerTrusted(r) {
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
