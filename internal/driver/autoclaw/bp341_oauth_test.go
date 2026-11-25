package autoclaw

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
)

// TestNewDeviceIDIsRandomPerSignIn is the multi-account guarantee.
//
// The device identity is whatever is presented at sign-in and the server
// records it in the token, so two accounts signed in with the same id are one
// device to the server — which is exactly how a second account can end up
// sharing (and exhausting) the first one's entitlements. Random per sign-in is
// what lets several accounts live on this machine.
//
// The shape matters too: 32 random bytes hex-encoded, 64 characters, which is
// what the official client sends.
func TestNewDeviceIDIsRandomPerSignIn(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := newDeviceID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 64 {
			t.Fatalf("device id %q has %d chars, want 64", id, len(id))
		}
		if strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("device id %q is not lowercase hex", id)
		}
		if seen[id] {
			t.Fatalf("device id repeated across two sign-ins: %s", id)
		}
		seen[id] = true
	}
}

// TestStateFromURL: the state the gateway puts in the authorize URL is the
// only thing that makes the callback check meaningful, so reading it must not
// silently return empty for a URL that has one.
func TestStateFromURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://accounts.google.com/o/oauth2/auth?client_id=x&state=abc123", "abc123"},
		{"https://accounts.google.com/o/oauth2/auth?state=abc&scope=y", "abc"},
		{"https://accounts.google.com/o/oauth2/auth?client_id=x", ""},
		{"no-query-at-all", ""},
	}
	for _, c := range cases {
		if got := stateFromURL(c.in); got != c.want {
			t.Errorf("stateFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCallbackRejectsMismatchedState: a code paired with a state we did not
// issue must be dropped, and the flow must stay open so the real callback can
// still land. Accepting it would let anyone sign this installation into an
// account they control.
func TestCallbackRejectsMismatchedState(t *testing.T) {
	f := &OAuthFlow{Vendor: "google", done: make(chan struct{})}
	f.state = "the-real-state"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback-google?code=attacker&state=wrong", nil)
	f.handleCallback(rec, req)

	if f.code != "" {
		t.Fatalf("a forged callback was accepted: code=%q", f.code)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("callback answered %d, want 200 with a refusal page", rec.Code)
	}

	// The legitimate callback still works afterwards.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/auth/callback-google?code=good&state=the-real-state", nil)
	f.handleCallback(rec2, req2)
	if f.code != "good" {
		t.Fatalf("the legitimate callback was refused after a forged one: code=%q", f.code)
	}
}

// TestCallbackBeforeOpenAuthIsRefused: with no state issued yet, any callback
// arriving is not ours.
func TestCallbackBeforeOpenAuthIsRefused(t *testing.T) {
	f := &OAuthFlow{Vendor: "zai", done: make(chan struct{})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback-zai?code=x&state=", nil)
	f.handleCallback(rec, req)
	if f.code != "" {
		t.Fatal("a callback was accepted before any sign-in was started")
	}
}

// TestRedirectURIUsesPoolPortAndLocalhost pins the two things the OAuth client
// validates. "127.0.0.1" instead of "localhost", or a port outside the pool,
// is refused upstream with "Redirect URI not registered for this client".
func TestRedirectURIUsesPoolPortAndLocalhost(t *testing.T) {
	f := &OAuthFlow{Vendor: "google", port: oauthPorts[0]}
	want := "http://localhost:18432/auth/callback-google"
	if got := f.redirectURI(); got != want {
		t.Fatalf("redirectURI = %q, want %q", got, want)
	}
	f.Vendor = "zai"
	if got := f.redirectURI(); !strings.HasSuffix(got, "/auth/callback-zai") {
		t.Fatalf("zai redirect = %q", got)
	}
}

// TestOAuthPortsAreTheOfficialPool: these are registered redirect targets, not
// a preference. A random port silently breaks sign-in.
func TestOAuthPortsAreTheOfficialPool(t *testing.T) {
	want := []int{18432, 19654, 19723, 53699}
	if len(oauthPorts) != len(want) {
		t.Fatalf("pool has %d ports, want %d", len(oauthPorts), len(want))
	}
	for i := range want {
		if oauthPorts[i] != want[i] {
			t.Fatalf("port %d = %d, want %d (order matters: the client tries them in turn)", i, oauthPorts[i], want[i])
		}
	}
}

// TestStartOAuthRejectsUnknownVendor: an unknown vendor would build a callback
// path no provider redirects to.
func TestStartOAuthRejectsUnknownVendor(t *testing.T) {
	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "test-secret"}, nil)
	if _, err := d.StartOAuth(context.Background(), "twitter"); err == nil {
		t.Fatal("an unknown vendor was accepted")
	}
}

// TestStartOAuthRejectsGoogle: Google sign-in was removed upstream (405 behind
// an edge block, confirmed against a real account 2026-11-25). z.ai is the
// only vendor left, so offering Google would walk the operator into a dead end.
// The error must say what to use instead, not just "invalid".
func TestStartOAuthRejectsGoogle(t *testing.T) {
	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "test-secret"}, nil)
	_, err := d.StartOAuth(context.Background(), "google")
	if err == nil {
		t.Fatal("google was accepted, but upstream no longer serves it")
	}
	if !strings.Contains(err.Error(), "z.ai") {
		t.Fatalf("the refusal must point at z.ai: %v", err)
	}
}

// TestStartOAuthServesGuideAndCaptchaConfig wires a real loopback listener and
// checks the guide page is reachable, then closes it. The captcha-config call
// needs the network, so it is expected to answer the "no slider" fallback
// rather than fail the request.
func TestStartOAuthServesGuideAndCaptchaConfig(t *testing.T) {
	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "test-secret"}, nil)
	f, err := d.StartOAuth(context.Background(), "google")
	if err != nil {
		t.Skipf("no loopback port available in this environment: %v", err)
	}
	defer f.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(f.URL)
	if err != nil {
		t.Fatalf("guide page unreachable at %s: %v", f.URL, err)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if !strings.Contains(string(body[:n]), "AutoClaw") {
		t.Fatalf("guide page does not look like ours: %q", string(body[:n]))
	}

	cfgResp, err := client.Post(f.URL+"captcha-config", "application/json", nil)
	if err != nil {
		t.Fatalf("captcha-config: %v", err)
	}
	defer cfgResp.Body.Close()
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("captcha-config answered %d", cfgResp.StatusCode)
	}
}

// TestPollTimeoutKeepsTheFlowAlive is the bug that made the panel unusable:
// the panel polls every two seconds with a three-second context, and the first
// implementation treated that timeout as "give up" and shut the listener down.
// The operator then saw "Failed to fetch" from the guide page and "ended
// without an authorization code" from the panel — while nothing had actually
// gone wrong.
//
// A poll timeout must report pending and leave the flow running, so a callback
// arriving later still completes the sign-in.
func TestPollTimeoutKeepsTheFlowAlive(t *testing.T) {
	f := &OAuthFlow{Vendor: "zai", done: make(chan struct{})}
	f.state = "wanted-state"

	// First poll times out, exactly as the panel's does.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := f.Wait(ctx)
	if !errors.Is(err, ErrOAuthPending) {
		t.Fatalf("a poll timeout returned %v, want ErrOAuthPending", err)
	}

	// The listener must still be usable: a callback that arrives now is
	// accepted, which is only possible if the flow was not torn down.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback-zai?code=late&state=wanted-state", nil)
	f.handleCallback(rec, req)

	f.mu.Lock()
	got := f.code
	f.mu.Unlock()
	if got != "late" {
		t.Fatalf("the flow was torn down by a poll timeout: code=%q", got)
	}
}

// TestStartOAuthSurvivesAPortPoolThatIsAllTaken is the field bug of
// 2026-11-25: the official client holds its own TokenServer ports whenever it
// is running (18432 by AutoClaw, 19723 by AutoClaw2 — verified with netstat),
// so binding only that pool made sign-in impossible on the very machine that
// has the client installed. A free ephemeral port must be the fallback.
func TestStartOAuthSurvivesAPortPoolThatIsAllTaken(t *testing.T) {
	// Stage "the pool is taken" with a pool we control, so the test does not
	// depend on whether the real client happens to be running.
	var taken []net.Listener
	prev := oauthPorts
	oauthPorts = nil
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		oauthPorts = append(oauthPorts, ln.Addr().(*net.TCPAddr).Port)
		taken = append(taken, ln)
	}
	t.Cleanup(func() {
		oauthPorts = prev
		for _, ln := range taken {
			ln.Close()
		}
	})

	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "test-secret"}, nil)
	f, err := d.StartOAuth(context.Background(), "zai")
	if err != nil {
		t.Fatalf("sign-in must still start when the official pool is taken: %v", err)
	}
	defer f.Close()

	if f.port == 0 {
		t.Fatal("no port was bound")
	}
	for _, p := range oauthPorts {
		if f.port == p {
			t.Fatalf("bound a pool port %d that we had just failed to get", f.port)
		}
	}
	// The redirect must name the port we actually bound, not a pool port.
	if want := fmt.Sprintf("http://localhost:%d/auth/callback-zai", f.port); f.redirectURI() != want {
		t.Fatalf("redirectURI = %q, want %q", f.redirectURI(), want)
	}
}

// TestSessionTimeoutReleasesThePort: an abandoned sign-in must not keep a
// listener bound for the gateway's whole lifetime. In the field a timed-out
// flow held port 19654 until the process was restarted, and 19654 belongs to
// the official client.
func TestSessionTimeoutReleasesThePort(t *testing.T) {
	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "test-secret"}, nil)
	// Drive the REAL timeout path with a short timeout: hand-calling Close
	// would hide whether the timeout itself releases the port, and that is
	// precisely the leak that shipped.
	f, err := d.StartOAuthWithTimeout(context.Background(), "zai", 60*time.Millisecond)
	if err != nil {
		t.Skipf("no loopback port available: %v", err)
	}
	port := f.port
	time.Sleep(250 * time.Millisecond) // let the session timer fire

	// The port must become bindable again. Shutdown closes the listener
	// asynchronously, so give it a moment rather than assuming it is instant —
	// what matters is that it comes back at all.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			ln.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d was not released after the flow ended: %v", port, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
