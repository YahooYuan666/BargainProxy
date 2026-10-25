package lobster

import (
	"strings"
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-110: the upstream answers a 5xx with an EMPTY body when it gives up on a
// heavy request. The driver worded every non-200 as "lobster: %s" around a
// truncated body, so an empty body produced the message "lobster: " — six legs
// logged exactly that on 2026-10-25 (4.1-8.9s each) and the logs could not tell
// a 500 from a 502, or an empty body from a driver that produced no message at
// all. The HTTP status was in ue.Status the whole time; it has to reach the
// message because the log line prints only the message.
func TestEmptyUpstreamBodyNamesTheStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  string
	}{
		{500, uperr.ClassTransport},
		{502, uperr.ClassTransport},
		{503, uperr.ClassTransport},
		{504, uperr.ClassTransport},
	} {
		e := classifyHTTP(tc.status, nil)
		if e.Class != tc.class {
			t.Fatalf("status %d class = %s, want %s", tc.status, e.Class, tc.class)
		}
		if e.Status != tc.status {
			t.Fatalf("status = %d, want %d", e.Status, tc.status)
		}
		want := "empty response body"
		if !strings.Contains(e.Message, want) {
			t.Fatalf("status %d message %q does not say the body was empty", tc.status, e.Message)
		}
		if !strings.Contains(e.Message, itoa(tc.status)) {
			t.Fatalf("status %d message %q does not name the HTTP status", tc.status, e.Message)
		}
		// The old shape ended at the colon with nothing after it.
		if strings.HasSuffix(strings.TrimSpace(e.Message), "lobster:") {
			t.Fatalf("status %d message still ends at the colon: %q", tc.status, e.Message)
		}
	}
}

// A body that DID say something must still be shown verbatim, not replaced by
// the empty-body wording.
func TestNonEmptyUpstreamBodyIsStillShown(t *testing.T) {
	e := classifyHTTP(500, []byte("upstream exploded"))
	if !strings.Contains(e.Message, "upstream exploded") {
		t.Fatalf("message %q dropped the upstream text", e.Message)
	}
	if strings.Contains(e.Message, "empty response body") {
		t.Fatalf("message %q claims an empty body for a non-empty one", e.Message)
	}
}

// A JSON business envelope riding a 5xx still wins: the code is authoritative,
// and the empty-body wording must not mask it.
func TestBusinessEnvelopeStillWins(t *testing.T) {
	e := classifyHTTP(500, []byte(`{"code":40100,"message":"token expired"}`))
	if e.Class != uperr.ClassAuth {
		t.Fatalf("business code on a 500 was not honoured: class=%s", e.Class)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
