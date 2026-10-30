package opencodezen

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// BP-220: the free lane reads the TLS client fingerprint. Go's crypto/tls
// ClientHello is answered 429 FreeUsageLimitError where Bun's passes — the
// exact same body and header values 200 from bun and 429 from Go across
// casing and HTTP/1.1 (2026-10-31 matrix). The sidecar is the final hop: the
// driver sends the built request over loopback to a Bun process, which
// re-sends it with Bun's own fetch and streams the answer back. Everything
// else (body build, headers, scheduling) stays in the gateway; when Bun is
// unavailable the driver falls back to direct transport and says so in the
// log — the lane then answers 429, but nothing else degrades.

//go:embed assets/zen_sidecar.ts
var zenSidecarScript []byte

// zenSidecar is one driver's sidecar transport.
type zenSidecar struct {
	mu      sync.Mutex
	baseURL string // "http://127.0.0.1:<port>" when running, "" when not
	cmd     *exec.Cmd
}

func (s *zenSidecar) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL != ""
}

func (s *zenSidecar) proxyURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseURL == "" {
		return ""
	}
	return s.baseURL + "/proxy"
}

// findBun locates a Bun runtime: the BP_BUN env var wins, then PATH, then the
// per-user location bun's own installer uses. Empty when none exists.
func findBun() string {
	if v := os.Getenv("BP_BUN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if p, err := exec.LookPath("bun"); err == nil {
		return p
	}
	name := "bun"
	if runtime.GOOS == "windows" {
		name = "bun.exe"
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".bun", "bin", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// startZenSidecar spawns one sidecar for the driver and health-checks it.
// Failures are logged once and the driver keeps working without it.
func startZenSidecar(d *Driver) {
	bun := findBun()
	if bun == "" {
		log.Printf("opencodezen: no bun runtime found (set BP_BUN); the free lane runs over Go's transport and the TLS gate may refuse it")
		return
	}
	dir, err := os.MkdirTemp("", "bp-zen-sidecar-")
	if err != nil {
		log.Printf("opencodezen: sidecar temp dir: %v", err)
		return
	}
	script := filepath.Join(dir, "zen_sidecar.ts")
	if err := os.WriteFile(script, zenSidecarScript, 0o600); err != nil {
		log.Printf("opencodezen: sidecar script: %v", err)
		return
	}
	// A free port, with the tiny close-to-bind race accepted (the health
	// check fails loudly if the sidecar loses it).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("opencodezen: sidecar port: %v", err)
		return
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command(bun, "run", script, fmt.Sprint(port))
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		log.Printf("opencodezen: sidecar start: %v", err)
		return
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// Health check: up to ~10s; bun's first run may still be compiling.
	alive := false
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		resp, err := (&http.Client{}).Do(req)
		cancel()
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			alive = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !alive {
		_ = cmd.Process.Kill()
		log.Printf("opencodezen: sidecar never came up on %s; falling back to direct transport", base)
		return
	}
	d.sidecar.mu.Lock()
	d.sidecar.baseURL = base
	d.sidecar.cmd = cmd
	d.sidecar.mu.Unlock()
	log.Printf("opencodezen: TLS sidecar up at %s (bun %s); the free lane's final hop speaks Bun", base, bun)
	go func() {
		_ = cmd.Wait()
		d.sidecar.mu.Lock()
		d.sidecar.baseURL = ""
		d.sidecar.mu.Unlock()
		log.Printf("opencodezen: sidecar exited; lane fell back to direct transport")
	}()
}

// zenRoundTrip sends one built request through the sidecar when it runs,
// else straight to the upstream with the given client. The returned response
// streams SSE either way.
func (d *Driver) zenRoundTrip(client *http.Client, req *http.Request) (*http.Response, error) {
	if proxy := d.sidecar.proxyURL(); proxy != "" {
		target := req.URL.String()
		proxied := req.Clone(req.Context())
		u, err := url.Parse(proxy)
		if err != nil {
			// The proxy URL is our own loopback string; never malformed in practice.
			proxied.Header.Set("x-bp-target", target)
		} else {
			proxied.URL = u
			proxied.Host = u.Host
			proxied.Header.Set("x-bp-target", target)
		}
		resp, err := client.Do(proxied)
		if err == nil {
			return resp, nil
		}
		// Sidecar hiccough (restart in progress?): fall through and try direct
		// once, so a dead sidecar costs one attempt, not the lane.
		log.Printf("opencodezen: sidecar hop failed (%v); trying direct", err)
	}
	return client.Do(req)
}
