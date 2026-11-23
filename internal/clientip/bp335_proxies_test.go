// BP-335: X-Forwarded-For must be honoured only from peers allowed to append
// it. With forwarding on and no allowlist, any caller that reaches the port
// chooses its own source IP — and therefore its own login rate-limit bucket
// (Coding2API 21879bdd, 2026-11-23).
package clientip

import (
	"net/http"
	"testing"
)

func req(remote, xff string) *http.Request {
	r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func reset(t *testing.T) {
	t.Helper()
	SetTrustForwarded(false)
	if err := SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
}

// With forwarding off, XFF is never read (the old default).
func TestXFFIgnoredWhenNotTrusted(t *testing.T) {
	reset(t)
	if got := From(req("192.0.2.5:1234", "203.0.113.9")); got != "192.0.2.5" {
		t.Fatalf("got %q, want the socket peer", got)
	}
}

// Trusted with NO allowlist keeps the old behaviour: any peer's XFF is used.
func TestXFFTrustedWithoutAllowlist(t *testing.T) {
	reset(t)
	SetTrustForwarded(true)
	if got := From(req("192.0.2.5:1234", "203.0.113.9")); got != "203.0.113.9" {
		t.Fatalf("got %q, want the XFF hop", got)
	}
}

// Trusted WITH an allowlist: a peer on the list may set XFF.
func TestXFFFromTrustedPeer(t *testing.T) {
	reset(t)
	SetTrustForwarded(true)
	if err := SetTrustedProxies([]string{"192.0.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if got := From(req("192.0.2.5:1234", "203.0.113.9")); got != "203.0.113.9" {
		t.Fatalf("got %q, want the XFF hop from a trusted peer", got)
	}
}

// A peer NOT on the list cannot forge XFF — the socket address wins.
func TestXFFFromUntrustedPeerIgnored(t *testing.T) {
	reset(t)
	SetTrustForwarded(true)
	if err := SetTrustedProxies([]string{"192.0.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if got := From(req("198.51.100.7:1234", "203.0.113.9")); got != "198.51.100.7" {
		t.Fatalf("got %q, want the socket peer (untrusted peers must not forge XFF)", got)
	}
}

// A bare IP (no /mask) is accepted as a single-host range.
func TestTrustedProxyBareIP(t *testing.T) {
	reset(t)
	SetTrustForwarded(true)
	if err := SetTrustedProxies([]string{"192.0.2.5"}); err != nil {
		t.Fatal(err)
	}
	if got := From(req("192.0.2.5:1", "203.0.113.9")); got != "203.0.113.9" {
		t.Fatalf("a bare IP must be accepted as a single host, got %q", got)
	}
	if got := From(req("192.0.2.6:1", "203.0.113.9")); got != "192.0.2.6" {
		t.Fatalf("a neighbour address must not be trusted, got %q", got)
	}
}

// IPv6 works too.
func TestTrustedProxyIPv6(t *testing.T) {
	reset(t)
	SetTrustForwarded(true)
	if err := SetTrustedProxies([]string{"2001:db8::/32"}); err != nil {
		t.Fatal(err)
	}
	if got := From(req("[2001:db8::1]:443", "203.0.113.9")); got != "203.0.113.9" {
		t.Fatalf("IPv6 trusted peer must be recognised, got %q", got)
	}
}

// A malformed entry is an error, not a silent "trust nobody".
func TestMalformedEntryErrors(t *testing.T) {
	reset(t)
	if err := SetTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Fatal("a malformed allowlist entry must be rejected at startup")
	}
}
