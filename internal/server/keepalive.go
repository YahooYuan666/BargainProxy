package server

// Stream keepalive: the client must hear SOMETHING while the upstream is
// silent, or its own watchdog cuts the turn.
//
// Why this exists (yetone/magpie 13f658a, 2026-10-27): a translated stream
// only spoke when the provider did. A provider that thinks for minutes and
// sends the whole reasoning step at the end sends nothing at all in between,
// and Claude Code's 300s event watchdog then cancels the request (field:
// 499 at ~357s and ~795s with the provider still open). Our own numbers say
// the same silence is normal here: qoder large-conversation p95 waits 12.9s,
// AutoClaw's relay answers in 5-38s, and a 560k-token prefill measured ~50s.
//
// Two hard protocol rules decide the shape:
//
//  1. Before the protocol's first event (Anthropic message_start, Responses
//     response.created, the Chat role chunk) NOTHING may be sent except an
//     SSE comment: Anthropic's own SDK throws "Unexpected event order, got
//     ping before message_start" on any event frame there.
//  2. Anthropic `ping` events are rate limited: Claude Code's watchdog stops
//     being reset after 30 pings in a row, so one ping per upstream keepalive
//     burns the whole allowance in minutes. At most one per minute, with SSE
//     comments filling the gaps.
//
// Keepalives never travel through the gateway's emitter, so they cannot make
// the BP-118 "the client has seen content" test pass: a silent provider must
// keep both escape hatches (cross-channel cascade and same-account
// continuation) open.

import (
	"io"
	"net/http"
	"sync"
	"time"
)

var (
	// keepaliveFirstGap is how long the FIRST silence may last before the
	// first keepalive. BP-198 (magpie c3462d5): Windows-local TUN/Clash-class
	// proxies cut an idle stream at ~10s (499/connection reset), and the cut
	// lands in the first silence — a deep-thinking model's TTFB alone can
	// exceed it. 3s proves the connection alive well before that.
	keepaliveFirstGap = 3 * time.Second
	// keepaliveGap is how long the stream may stay silent between keepalives
	// after the first. Well under every client watchdog we know of (Claude
	// Code 300s) and under typical intermediate proxy idle timeouts; 15s
	// used to sit ABOVE the ~10s local-proxy cut (BP-198).
	keepaliveGap = 5 * time.Second
	// keepalivePingGap is the minimum spacing between Anthropic ping events.
	keepalivePingGap = time.Minute
	// keepaliveLifetime bounds the whole thing: a provider that never
	// answers must still end, so silence is kept alive for this long and
	// then left to the upstream watchdogs (magpie's translatedQuietLongest).
	keepaliveLifetime = 30 * time.Minute
)

// lockedWriter serialises every write to the response body.
//
// The keepalive ticker and the protocol stream writer both write, from
// different goroutines. One frame must never be split by another, so both go
// through this mutex. Each Write flushes, matching what the stream writers
// already did per event.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
	fl http.Flusher
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.w.Write(b)
	if l.fl != nil {
		l.fl.Flush()
	}
	return n, err
}

// Flush exists so the stream writers can keep their `interface{ Flush() }`
// shape; it is a no-op because Write already flushed.
func (l *lockedWriter) Flush() {}

func (l *lockedWriter) writeString(s string) error {
	_, err := l.Write([]byte(s))
	return err
}

// streamKeepalive writes SSE comments (and, where the protocol allows it,
// ping events) while the upstream is quiet.
type streamKeepalive struct {
	lw        *lockedWriter
	pingFrame string // "" = comments only; otherwise the frame used once started

	mu       sync.Mutex
	started  bool
	lastEvt  time.Time
	lastPing time.Time

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// newStreamKeepalive wraps w so all writes are serialised, and starts the
// silence ticker. pingFrame is the Anthropic `event: ping` block for the
// Anthropic ingress and "" for the protocols where a comment is the only
// keepalive their parsers tolerate (Responses sequence numbers and Chat
// chunks must not be invented by us).
func newStreamKeepalive(w http.ResponseWriter, pingFrame string) (*lockedWriter, *streamKeepalive) {
	lw := &lockedWriter{w: w}
	if fl, ok := w.(http.Flusher); ok {
		lw.fl = fl
	}
	ka := &streamKeepalive{
		lw:        lw,
		pingFrame: pingFrame,
		lastEvt:   time.Now(),
		lastPing:  time.Now(), // do not ping immediately on a started stream
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go ka.loop()
	return lw, ka
}

// MarkStarted records that the protocol's first event has been written, so
// later keepalives may use the protocol's own keepalive frame instead of a
// bare comment.
func (k *streamKeepalive) MarkStarted() {
	k.mu.Lock()
	k.started = true
	k.mu.Unlock()
}

// NoteEvent records real output so the next keepalive waits a full gap again.
func (k *streamKeepalive) NoteEvent() {
	k.mu.Lock()
	k.lastEvt = time.Now()
	k.mu.Unlock()
}

// Stop ends the ticker and waits for it to exit. Safe to call more than once.
func (k *streamKeepalive) Stop() {
	k.stopOnce.Do(func() { close(k.stop) })
	<-k.done
}

func (k *streamKeepalive) loop() {
	defer close(k.done)
	deadline := time.Now().Add(keepaliveLifetime)
	// The first keepalive comes sooner than the steady gap (BP-198): the
	// ~10s idle cut of local proxies lands in the first silence, before any
	// frame has flowed.
	if !k.silenceTick(keepaliveFirstGap, &deadline) {
		return
	}
	t := time.NewTimer(keepaliveGap)
	defer t.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-t.C:
		}
		if !k.silenceTick(keepaliveGap, &deadline) {
			return
		}
		t.Reset(keepaliveGap)
	}
}

// silenceTick waits one gap, then writes the keepalive if the stream has
// been silent for that long. It returns false when the loop must end:
// stopped, lifetime over, or the write failed (client gone).
func (k *streamKeepalive) silenceTick(gap time.Duration, deadline *time.Time) bool {
	t := time.NewTimer(gap)
	defer t.Stop()
	select {
	case <-k.stop:
		return false
	case <-t.C:
	}
	now := time.Now()
	if now.After(*deadline) {
		return false
	}
	k.mu.Lock()
	silent := now.Sub(k.lastEvt) >= gap
	started := k.started
	pingDue := started && k.pingFrame != "" && now.Sub(k.lastPing) >= keepalivePingGap
	if pingDue {
		k.lastPing = now
	}
	if silent {
		k.lastEvt = now
	}
	k.mu.Unlock()
	if !silent {
		return true
	}
	frame := ": keepalive\n\n"
	if pingDue {
		frame = k.pingFrame
	}
	if err := k.lw.writeString(frame); err != nil {
		return false // client is gone; nothing left to keep alive
	}
	return true
}
