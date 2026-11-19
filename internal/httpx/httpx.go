// Package httpx centralizes upstream HTTP client construction and the
// streaming idle watchdog (ticket 27).
//
// Two client shapes:
//   - Chat: no total timeout (streams can legitimately run for many minutes);
//     the response-header timeout bounds the wait for the upstream to start
//     answering, and WatchIdle bounds quiet gaps mid-stream.
//   - RPC: short billing/refresh/probe calls get a total timeout.
//
// Process-wide defaults are configured once at startup from config.json;
// tests may call Configure with small values (and restore afterwards).
package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/uperr"
)

// Default upstream timeouts (agentpoints parity: 120s short RPC, 300s to
// first byte, 300s idle mid-stream).
const (
	DefaultRPCTimeout    = 120 * time.Second
	DefaultHeaderTimeout = 300 * time.Second
	DefaultIdleTimeout   = 300 * time.Second
)

var (
	mu            sync.RWMutex
	rpcTimeout    = DefaultRPCTimeout
	headerTimeout = DefaultHeaderTimeout
	idleTimeout   = DefaultIdleTimeout
)

// Configure sets the process-wide upstream timeouts (<=0 keeps the default).
func Configure(rpc, header, idle time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	if rpc > 0 {
		rpcTimeout = rpc
	}
	if header > 0 {
		headerTimeout = header
	}
	if idle > 0 {
		idleTimeout = idle
	}
}

// RPCTimeout / HeaderTimeout / IdleTimeout return the current defaults.
func RPCTimeout() time.Duration {
	mu.RLock()
	defer mu.RUnlock()
	return rpcTimeout
}

func HeaderTimeout() time.Duration {
	mu.RLock()
	defer mu.RUnlock()
	return headerTimeout
}

func IdleTimeout() time.Duration {
	mu.RLock()
	defer mu.RUnlock()
	return idleTimeout
}

// ChatClient builds a client for streaming chat calls: no total timeout,
// response-header timeout from the current default.
func ChatClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = HeaderTimeout()
	return &http.Client{Transport: tr}
}

// RPCClient builds a client for short billing/refresh/probe calls.
func RPCClient() *http.Client {
	return &http.Client{Timeout: RPCTimeout()}
}

// h1Transport clones the default transport with HTTP/2 disabled.
//
// BP-331 (CLIProxyAPI d0c90bb5): an h2 connection to slow Google endpoints is
// what produced the BP-256 field hang (`http2: timeout awaiting response
// headers`, 6m40s). The pinning was wired for antigravity and REVERTED within
// minutes: through the local Mihomo TUN egress the antigravity quota probes
// answered `malformed HTTP response … http2_handshake_failed` on every call —
// in THIS network environment those endpoints need h2, and the hang class
// stays handled by the host deadline. The helpers stay available (and tested)
// for an endpoint family that actually accepts h1; nothing wires them.
// An empty non-nil TLSNextProto is the documented way to keep the transport
// from registering its h2 upgrade; ForceAttemptHTTP2=false backs it up.
func h1Transport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	return tr
}

// ChatClientH1 is ChatClient pinned to HTTP/1.1 (see h1Transport).
func ChatClientH1() *http.Client {
	tr := h1Transport()
	tr.ResponseHeaderTimeout = HeaderTimeout()
	return &http.Client{Transport: tr}
}

// RPCClientH1 is RPCClient pinned to HTTP/1.1 (see h1Transport).
func RPCClientH1() *http.Client {
	return &http.Client{Timeout: RPCTimeout(), Transport: h1Transport()}
}

// ApplyRetryAfter attaches the response's Retry-After header to a classified
// upstream error as its reset moment.
//
// BP-331 (CLIProxyAPI deacbf21): the body-parse paths (qoder retryAfterSeconds,
// workbuddy data.retry_after, antigravity QuotaFailure) each carry their own
// reset signal, but a bare header 429/503 used to fall through to the account's
// backoff ladder even when the upstream stated exactly when to come back. The
// pool already consumes Error.ResetAt for both classes (pool.go soft-rate
// branch), so this only fills it in. Applies ONLY when the class benefits and
// nothing more precise was already parsed from the body.
func ApplyRetryAfter(e *uperr.Error, h http.Header) {
	if e == nil || h == nil {
		return
	}
	if e.Class != uperr.ClassSoftRate && e.Class != uperr.ClassUpstreamBusy {
		return
	}
	if !e.ResetAt.IsZero() {
		return
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		e.ResetAt = time.Now().Add(time.Duration(secs) * time.Second)
		return
	}
	if t, err := http.ParseTime(v); err == nil {
		e.ResetAt = t
	}
}

// ClassifyTransportErr maps a client.Do / stream-read failure to an uperr
// class:
//
//   - ctx already done → ClassCanceled (client disconnect, fallback window,
//     shutdown — never the account's fault);
//   - net timeout / explicit deadline → ClassUpstreamTimeout (the upstream
//     stalled: rotating accounts hits the same slow upstream);
//   - anything else → ClassTransport (network jitter).
func ClassifyTransportErr(ctx context.Context, err error) string {
	if ctx != nil && ctx.Err() != nil {
		return uperr.ClassCanceled
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return uperr.ClassUpstreamTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return uperr.ClassUpstreamTimeout
	}
	if IsPersistentTransportErr(err) {
		return uperr.ClassTransportDead
	}
	return uperr.ClassTransport
}

// persistentTransportMarkers are failures that retrying cannot fix. They are
// properties of the egress path (proxy credentials, DNS, routing), not of one
// account or one request, so every account of the channel fails identically.
//
// Field motivation: an expired SOCKS5 proxy subscription made every request
// hard-fail; without this split the gateway kept re-entering an 80s backoff
// loop forever and never reported the real cause (BP-99, sub2api parity).
var persistentTransportMarkers = []string{
	"proxy authentication required", // 407 from the proxy
	"proxyconnect",                  // proxy handshake failure
	"no such host",                  // DNS cannot resolve
	"no route to host",              // routing table / wrong gateway
	"network is unreachable",        // adapter down / no default route
	"connection refused",            // nothing listening (dead local relay)
	// BP-331 (magpie 6ac19f4b): Windows WSAECONNREFUSED spells "No connection
	// could be made because the target machine ACTIVELY REFUSED it" — no
	// "connection refused" substring anywhere. This host IS Windows, so a dead
	// local relay used to fall through to the transient-bucket retry ladder.
	"actively refused",      // Windows wording of the same refusal
	"authentication failed", // egress auth in general
}

// IsPersistentTransportErr reports whether a transport error is of the kind
// that will not heal by retrying. Callers use it to park the channel instead of
// walking the pool.
func IsPersistentTransportErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range persistentTransportMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// IdleBody wraps a streaming response body with an idle watchdog: when no
// Read makes progress for the idle window the body is closed (pending and
// subsequent Reads fail) and Fired reports true, letting the driver classify
// the failure as an upstream stall.
type IdleBody struct {
	rc    io.ReadCloser
	d     time.Duration
	timer *time.Timer

	mu    sync.Mutex
	fired bool
}

// WatchIdle wraps rc; d <= 0 disables the watchdog (returns rc unchanged
// semantics, still type-assertable).
func WatchIdle(rc io.ReadCloser, d time.Duration) *IdleBody {
	b := &IdleBody{rc: rc, d: d}
	if d > 0 {
		b.timer = time.AfterFunc(d, func() {
			b.mu.Lock()
			b.fired = true
			b.mu.Unlock()
			_ = rc.Close() // unblocks a pending Read
		})
	}
	return b
}

func (b *IdleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 || err != nil {
		if b.timer != nil {
			b.timer.Reset(b.d)
		}
	}
	return n, err
}

func (b *IdleBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	return b.rc.Close()
}

// Fired reports whether the idle watchdog closed the stream.
func (b *IdleBody) Fired() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fired
}

// IdleFired reports whether v is (or wraps as) an IdleBody that fired — for
// callers that only hold the body as io.Reader / io.ReadCloser.
func IdleFired(v any) bool {
	if b, ok := v.(*IdleBody); ok {
		return b.Fired()
	}
	return false
}
