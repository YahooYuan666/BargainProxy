package httpx

// BP-331: three borrowed fixes — the Windows refusal wording (magpie
// 6ac19f4b), the h1-pinned transport (CLIProxyAPI d0c90bb5), and the
// Retry-After header attached to the classified error (CLIProxyAPI
// deacbf21).

import (
	"net/http"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// Windows WSAECONNREFUSED has no "connection refused" substring — the marker
// list must still catch it. Mutation: drop the marker and this must fail.
func TestWindowsActivelyRefusedIsPersistent(t *testing.T) {
	err := &netOpError{text: "dial tcp 127.0.0.1:8119: connectex: No connection could be made because the target machine actively refused it."}
	if !IsPersistentTransportErr(err) {
		t.Fatal("Windows 'actively refused' wording must classify as persistent (BP-331, magpie 6ac19f4b)")
	}
	// And the Linux wording keeps working.
	if !IsPersistentTransportErr(&netOpError{text: "dial tcp 127.0.0.1:8119: connect: connection refused"}) {
		t.Fatal("plain 'connection refused' must stay persistent")
	}
	if IsPersistentTransportErr(&netOpError{text: "read: connection reset by peer"}) {
		t.Fatal("transient wording must stay transient")
	}
}

// netOpError is a minimal error carrier; errors.As for net.Error must not
// match it (it is not a timeout), so the marker scan is what decides.
type netOpError struct{ text string }

func (e *netOpError) Error() string { return e.text }

// Retry-After delta-seconds form lands in ResetAt for the classes the pool
// consumes it for.
func TestApplyRetryAfterDeltaSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "37")
	e := uperr.New(uperr.ClassSoftRate, 429, "x")
	ApplyRetryAfter(e, h)
	if e.ResetAt.IsZero() {
		t.Fatal("ResetAt must be set from the header")
	}
	if d := time.Until(e.ResetAt); d < 30*time.Second || d > 45*time.Second {
		t.Fatalf("ResetAt = %v from now, want ~37s", d)
	}
}

// HTTP-date form parses too.
func TestApplyRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{}
	want := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	h.Set("Retry-After", want.Format(http.TimeFormat))
	e := uperr.New(uperr.ClassUpstreamBusy, 503, "x")
	ApplyRetryAfter(e, h)
	if got := e.ResetAt.Truncate(time.Second); !got.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v", got, want)
	}
}

// A body-parsed reset (already precise) wins; other classes are untouched.
func TestApplyRetryAfterGuards(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "99")
	e := uperr.New(uperr.ClassSoftRate, 429, "x")
	e.ResetAt = time.Now().Add(5 * time.Second)
	ApplyRetryAfter(e, h)
	if d := time.Until(e.ResetAt); d > 10*time.Second {
		t.Fatal("an already-parsed ResetAt must not be overwritten")
	}

	bad := uperr.New(uperr.ClassBadRequest, 400, "x")
	ApplyRetryAfter(bad, h)
	if !bad.ResetAt.IsZero() {
		t.Fatal("ClassBadRequest must not take a cooldown from the header")
	}
	ApplyRetryAfter(nil, h) // must not panic
	empty := uperr.New(uperr.ClassSoftRate, 429, "x")
	ApplyRetryAfter(empty, http.Header{})
	if !empty.ResetAt.IsZero() {
		t.Fatal("no header must leave ResetAt alone")
	}
}

// The h1-pinned transport really pins h1: no h2 upgrade registered, no
// ForceAttemptHTTP2, and NO ALPN extension at all (BP-332 — the native
// Antigravity client's wire shape; advertising http/1.1 would be a different
// fingerprint).
func TestChatClientH1PinsHTTP1(t *testing.T) {
	tr := h1Transport()
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must be off")
	}
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Fatal("TLSNextProto must be an empty non-nil map (disables the h2 upgrade)")
	}
	// BP-332: the line BP-331 missed. Without an explicit TLSClientConfig with
	// NextProtos=nil the handshake still advertises h2 and every antigravity
	// probe died with "malformed HTTP response … http2_handshake_failed".
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig must be explicit — a nil config leaves the ALPN shape implicit (BP-332)")
	}
	if len(tr.TLSClientConfig.NextProtos) != 0 {
		t.Fatalf("NextProtos = %v, want nil/empty (native Antigravity sends no ALPN; http/1.1 would be a third fingerprint)", tr.TLSClientConfig.NextProtos)
	}
	if ChatClientH1() == nil || RPCClientH1() == nil {
		t.Fatal("clients must construct")
	}
	// The clone must not mutate the process-wide default transport's config.
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base.TLSClientConfig != nil &&
		len(base.TLSClientConfig.NextProtos) == 0 && base.TLSClientConfig == tr.TLSClientConfig {
		t.Fatal("TLSClientConfig must be cloned, never shared with http.DefaultTransport")
	}
}
