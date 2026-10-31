package clientip

import (
	"net/http"
	"testing"
)

// BP-246: X-Forwarded-For is caller-controlled. Trusting it by default let any
// client choose the source IP in the request log and — worse — rotate its own
// bucket in the panel's login rate limiter.
func TestForwardedIgnoredByDefault(t *testing.T) {
	SetTrustForwarded(false)
	defer SetTrustForwarded(false)

	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := From(r); got != "127.0.0.1" {
		t.Fatalf("From = %q, want the socket address 127.0.0.1; a forged XFF was believed", got)
	}
}

// With a trusted proxy in front, the RIGHTMOST hop is used: the leftmost is
// whatever the client typed, the rightmost is what the last proxy observed.
func TestForwardedUsesRightmostHopWhenTrusted(t *testing.T) {
	SetTrustForwarded(true)
	defer SetTrustForwarded(false)

	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if got := From(r); got != "203.0.113.9" {
		t.Fatalf("From = %q, want the rightmost hop 203.0.113.9", got)
	}
}

// A request with no parseable RemoteAddr must still yield something non-empty:
// an empty string would collapse every caller into one rate-limit bucket.
func TestUnparseableRemoteAddrIsNotDropped(t *testing.T) {
	SetTrustForwarded(false)
	defer SetTrustForwarded(false)

	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "not-a-host-port"
	if got := From(r); got == "" {
		t.Fatal("From returned empty for an unparseable RemoteAddr")
	}
}

func TestNilRequestIsSafe(t *testing.T) {
	if got := From(nil); got != "" {
		t.Fatalf("From(nil) = %q, want empty", got)
	}
}
