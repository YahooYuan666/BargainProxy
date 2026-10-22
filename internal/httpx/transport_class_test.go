package httpx

import (
	"context"
	"errors"
	"net"
	"testing"

	"bargainproxy/internal/uperr"
)

// TestPersistentTransportErrorsAreSplit pins the split between "retry will fix
// it" and "retry can never fix it". Without it a dead proxy credential makes
// every request re-enter an 80s backoff loop forever instead of parking the
// channel and reporting the real cause (BP-99).
func TestPersistentTransportErrorsAreSplit(t *testing.T) {
	persistent := []error{
		errors.New(`Post "https://x/v1/chat": proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: connection refused`),
		errors.New(`Proxy Authentication Required`),
		errors.New(`dial tcp: lookup api.example.com: no such host`),
		errors.New(`dial tcp 10.0.0.1:443: connect: no route to host`),
		errors.New(`dial tcp: connect: network is unreachable`),
		errors.New(`authentication failed`),
	}
	for _, err := range persistent {
		if got := ClassifyTransportErr(context.Background(), err); got != uperr.ClassTransportDead {
			t.Errorf("%v → %q, want %q", err, got, uperr.ClassTransportDead)
		}
	}

	// Transient problems must stay on the ordinary backoff path.
	transient := []error{
		errors.New(`read tcp: connection reset by peer`),
		errors.New(`unexpected EOF`),
		errors.New(`http2: stream closed`),
	}
	for _, err := range transient {
		if got := ClassifyTransportErr(context.Background(), err); got != uperr.ClassTransport {
			t.Errorf("%v → %q, want %q", err, got, uperr.ClassTransport)
		}
	}
}

// TestTransportDeadIsNotRotatable: switching accounts cannot fix an egress
// problem, so the binding loop must not walk the pool.
func TestTransportDeadIsNotRotatable(t *testing.T) {
	if uperr.Rotatable(uperr.ClassTransportDead) {
		t.Fatal("transport_dead must not be rotatable across accounts")
	}
}

// TestTimeoutStillWins: an error that is both a timeout and matches a
// persistent marker must be classified as a timeout (the timeout branch is
// checked first, and a stalled upstream is not a dead egress path).
func TestTimeoutStillWins(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "x", IsTimeout: true}
	if got := ClassifyTransportErr(context.Background(), err); got != uperr.ClassUpstreamTimeout {
		t.Fatalf("timeout must win: got %q", got)
	}
}

// TestCanceledWinsOverEverything: a cancelled context is our own doing.
func TestCanceledWinsOverEverything(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ClassifyTransportErr(ctx, errors.New("connection refused")); got != uperr.ClassCanceled {
		t.Fatalf("got %q, want canceled", got)
	}
}
