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
	"errors"
	"io"
	"net"
	"net/http"
	"os"
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
	return uperr.ClassTransport
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
