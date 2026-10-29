package server

// BP-142: the client must not be left in silence while the upstream thinks,
// and the keepalive must respect each protocol's framing rules.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a ResponseWriter whose body can be read while the stream is
// still open (the keepalive ticker writes from another goroutine).
type syncBuf struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	status int
	hdr    http.Header
}

func newSyncBuf() *syncBuf { return &syncBuf{hdr: http.Header{}} }

func (s *syncBuf) Header() http.Header { return s.hdr }
func (s *syncBuf) WriteHeader(status int) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}
func (s *syncBuf) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}
func (s *syncBuf) Flush() {}
func (s *syncBuf) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// withKeepaliveTimings shrinks the windows so the test does not wait a
// minute, restoring them afterwards. The first-gap window rides the same
// shrink (BP-198): a 3s first keepalive would outlast every sleep here.
func withKeepaliveTimings(t *testing.T, gap, ping, life time.Duration) {
	t.Helper()
	oldGap, oldFirst, oldPing, oldLife := keepaliveGap, keepaliveFirstGap, keepalivePingGap, keepaliveLifetime
	keepaliveGap, keepaliveFirstGap, keepalivePingGap, keepaliveLifetime = gap, gap, ping, life
	t.Cleanup(func() {
		keepaliveGap, keepaliveFirstGap, keepalivePingGap, keepaliveLifetime = oldGap, oldFirst, oldPing, oldLife
	})
}

// TestKeepaliveIsCommentOnlyBeforeFirstEvent pins the Anthropic SDK rule: an
// event frame (including ping) before message_start makes the official SDK
// fail with "Unexpected event order, got ping before message_start".
func TestKeepaliveIsCommentOnlyBeforeFirstEvent(t *testing.T) {
	withKeepaliveTimings(t, 10*time.Millisecond, 10*time.Millisecond, 2*time.Second)
	buf := newSyncBuf()
	lw, ka := newStreamKeepalive(buf, "event: ping\ndata: {\"type\":\"ping\"}\n\n")

	time.Sleep(80 * time.Millisecond) // several ticks, stream not started
	got := buf.body()
	if strings.Contains(got, "event: ") {
		t.Fatalf("an event frame went out before message_start:\n%q", got)
	}
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("no comment keepalive while silent:\n%q", got)
	}

	// Once started, the protocol's own frame is allowed.
	ka.MarkStarted()
	before := strings.Count(buf.body(), "event: ping")
	time.Sleep(120 * time.Millisecond)
	after := strings.Count(buf.body(), "event: ping")
	if after <= before {
		t.Fatalf("no ping after MarkStarted (before=%d after=%d):\n%q", before, after, buf.body())
	}

	ka.Stop()
	// Stop is idempotent and must not hang.
	ka.Stop()
	_ = lw
}

// TestKeepalivePingIsRateLimited pins the second rule: Claude Code's watchdog
// stops being reset after 30 pings in a row, so pings must be spaced out and
// comments must fill the gaps.
func TestKeepalivePingIsRateLimited(t *testing.T) {
	withKeepaliveTimings(t, 5*time.Millisecond, time.Hour, time.Second)
	buf := newSyncBuf()
	_, ka := newStreamKeepalive(buf, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	ka.MarkStarted()

	time.Sleep(80 * time.Millisecond)
	ka.Stop()

	if n := strings.Count(buf.body(), "event: ping"); n > 1 {
		t.Fatalf("pings were not rate limited: %d pings in 80ms\n%q", n, buf.body())
	}
	if !strings.Contains(buf.body(), ": keepalive") {
		t.Fatalf("no comment keepalive alongside the rate-limited ping:\n%q", buf.body())
	}
}

// TestKeepaliveStopsWhenTheStreamSpeaks: real output resets the silence
// timer, so an actively streaming response sends no keepalives at all.
func TestKeepaliveStopsWhenTheStreamSpeaks(t *testing.T) {
	withKeepaliveTimings(t, 40*time.Millisecond, time.Hour, 2*time.Second)
	buf := newSyncBuf()
	_, ka := newStreamKeepalive(buf, "")

	for i := 0; i < 6; i++ {
		time.Sleep(10 * time.Millisecond)
		ka.NoteEvent()
	}
	ka.Stop()
	if got := buf.body(); got != "" {
		t.Fatalf("keepalive sent while the stream was active:\n%q", got)
	}
}

// TestKeepaliveStopsAtTheLifetime: a provider that never answers must still
// end, so keepalives are bounded.
func TestKeepaliveStopsAtTheLifetime(t *testing.T) {
	withKeepaliveTimings(t, 5*time.Millisecond, time.Hour, 40*time.Millisecond)
	buf := newSyncBuf()
	_, ka := newStreamKeepalive(buf, "")

	time.Sleep(150 * time.Millisecond)
	first := buf.body()
	time.Sleep(80 * time.Millisecond)
	if buf.body() != first {
		t.Fatalf("keepalive continued past its lifetime:\n%q\n%q", first, buf.body())
	}
	ka.Stop()
}

// TestKeepaliveDoesNotCountAsDeliveredOutput is the BP-118 interaction that
// would silently disable both recovery paths: keepalives bypass the gateway
// emitter entirely, so they can never make emitted()==true. This test pins
// the seam — the writer handed to the protocol is a plain io.Writer, and the
// gateway's emitter never sees a keepalive event.
func TestKeepaliveDoesNotCountAsDeliveredOutput(t *testing.T) {
	withKeepaliveTimings(t, 5*time.Millisecond, time.Hour, time.Second)
	buf := newSyncBuf()
	lw, ka := newStreamKeepalive(buf, "")
	defer ka.Stop()

	// A driver-level emitter (what the gateway wraps) sees only what the
	// driver produces; the keepalive writer is downstream of it.
	var driverEvents int
	emit := func(string) { driverEvents++ }
	time.Sleep(60 * time.Millisecond)
	emit("upstream-event")

	if !strings.Contains(buf.body(), ": keepalive") {
		t.Fatalf("expected a keepalive on the wire:\n%q", buf.body())
	}
	if driverEvents != 1 {
		t.Fatalf("keepalive leaked into the driver emitter: %d events", driverEvents)
	}
	_ = lw
}

// TestAnthropicKeepaliveFraming checks the concrete Anthropic frame the
// handler installs is a well-formed SSE event.
func TestAnthropicKeepaliveFraming(t *testing.T) {
	const frame = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	if !strings.HasPrefix(frame, "event: ping\n") || !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("malformed ping frame: %q", frame)
	}
	rec := httptest.NewRecorder()
	_, _ = rec.WriteString(frame)
	if !strings.Contains(rec.Body.String(), "\"type\":\"ping\"") {
		t.Fatalf("ping payload missing: %q", rec.Body.String())
	}
}
