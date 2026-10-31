package antigravity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
)

// BP-258: the per-host deadline covers the wait for HEADERS, not the stream.
//
// The first cut of BP-256 wrapped the request in context.WithTimeout and did
// `defer cancelHost()`. doStream returns as soon as headers arrive, so the
// deferred cancel fired at that exact moment — and the streaming body is read
// by the caller under that same context, so every stream died 1-4s in with
// "context canceled" while the client was still connected:
//
//	2026/11/01 02:57:02 leg failed after 4.035s: class=transport status=0:
//	  antigravity: stream read: context canceled
//
// The faster the host returned headers, the faster the stream died, which made
// it look like a Gemini/Claude upstream regression instead of our own bug.
//
// This test fails on the old code: the server answers headers instantly and
// then sends one event per tick, so a context canceled at return time kills the
// read before the first event arrives.
func TestStreamSurvivesHeadersArrival(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers out now: this is what used to trigger the cancel
		}
		for i := 0; i < 3; i++ {
			time.Sleep(120 * time.Millisecond)
			fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"chunk%d\"}]}}]}\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	d := New(config.AntigravitySettings{
		Enabled:      true,
		BaseURL:      srv.URL,
		HostDeadline: config.Duration(5 * time.Second), // generous: only a hang should trip it
	}, &http.Client{})

	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	defer resp.Body.Close()

	// Read slowly enough that the old code's immediate cancel would have won.
	got := 0
	buf := make([]byte, 256)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		got += n
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("stream read failed after %d bytes: %v — the per-host "+
				"deadline canceled the body, it must only bound the header wait", got, rerr)
		}
	}
	if got == 0 {
		t.Fatal("read nothing from the stream")
	}
}

// A hang must still be cut: the header wait keeps its deadline.
func TestHeaderWaitStillCut(t *testing.T) {
	hung := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hung
	}))
	defer srv.Close()
	defer close(hung)

	d := New(config.AntigravitySettings{
		Enabled:      true,
		BaseURL:      srv.URL,
		HostDeadline: config.Duration(200 * time.Millisecond),
	}, &http.Client{})

	start := time.Now()
	_, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err == nil {
		t.Fatal("a hanging host with no fallback must return an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %s; the header deadline did not fire", elapsed)
	}
}

// The stream context must be released when the body is closed, or every request
// leaks a context and its timer.
func TestStreamContextReleasedOnClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	d := New(config.AntigravitySettings{
		Enabled:      true,
		BaseURL:      srv.URL,
		HostDeadline: config.Duration(30 * time.Second),
	}, &http.Client{})

	resp, err := d.doStream(context.Background(), "token", "gemini-3.8-flash-high", []byte(`{}`))
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	if _, ok := resp.Body.(*cancelOnClose); !ok {
		t.Fatalf("body is %T; the per-host cancel must be released on Close, "+
			"otherwise the context outlives the request", resp.Body)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
