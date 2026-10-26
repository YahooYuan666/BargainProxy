package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TRAE-2 red-first tests (BP-98): the check-in must go out looking like the
// IDE's growth page, or Trae CN answers 9074 no matter how seldom it is
// asked (yetone/magpie#808): Electron-shell UA, the device id, and
// {req_source: 1} instead of {}. The device id itself must be 16 digits
// (magpie-community/plugins#194: a 19-digit id is answered 9074 where the
// same account on a 16-digit one is taken).

// TestUgHeadersCarryDesktopIdentity: ugHeaders sends the Electron UA and the
// device id when the account has one.
func TestUgHeadersCarryDesktopIdentity(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://x/trae/api/v2/ug/checkin_credits/claim", nil)
	ugHeaders(req, &Creds{AccessToken: "jwt", DeviceID: "1234567890123456"})
	if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "TRAE-SOLO-CN/") || !strings.Contains(ua, "Electron/") {
		t.Fatalf("ug UA must be the Electron shell, got %q", ua)
	}
	if got := req.Header.Get("X-Device-Id"); got != "1234567890123456" {
		t.Fatalf("X-Device-Id = %q", got)
	}
	if req.Header.Get("X-User-Region") != "CN" {
		t.Fatal("X-User-Region must stay CN")
	}
}

// TestUgDeviceNormalizedTo16Digits: a stored 19-digit (or empty-with-uid)
// device id is mapped onto a stable 16-digit one, derived per account —
// never a new random device per request (magpie 0.2.19 ugDevice: a 9095 on
// it must stay the day's answer, so it may not rotate).
func TestUgDeviceNormalizedTo16Digits(t *testing.T) {
	d1 := ugDeviceID("8800000012345678901", "uid-1")
	if len(d1) != 16 {
		t.Fatalf("derived device id = %q, want 16 digits", d1)
	}
	for _, c := range d1 {
		if c < '0' || c > '9' {
			t.Fatalf("derived device id must be digits only, got %q", d1)
		}
	}
	// Stable across calls and accounts: same inputs -> same id; a different
	// uid -> a different id.
	if d1 != ugDeviceID("8800000012345678901", "uid-1") {
		t.Fatal("device id derivation must be stable")
	}
	if d1 == ugDeviceID("8800000012345678901", "uid-2") {
		t.Fatal("different uids must derive different device ids")
	}
	// An id already in the client's shape passes through untouched.
	if got := ugDeviceID("1234567890123456", "uid-1"); got != "1234567890123456" {
		t.Fatalf("client-shaped id must pass through, got %q", got)
	}
}

// TestUgBodyReqSource: the ug endpoints get {"req_source":1}, not {} —
// every check-in sent with {} came back 9074 even with the IDE's headers
// (yetone/magpie#808).
func TestUgBodyReqSource(t *testing.T) {
	var statusBody, claimBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 512)
		n, _ := r.Body.Read(raw)
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			statusBody = string(raw[:n])
			fmt.Fprint(w, `{"checked_in":false,"enable":true,"credits":100}`)
		default:
			claimBody = string(raw[:n])
			fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1", "device_id": "1234567890123456"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	ok, _, _, err := d.Checkin(context.Background(), &model.Account{ID: "trae:u1", CredsJSON: creds})
	if err != nil || !ok {
		t.Fatalf("checkin: ok=%v err=%v", ok, err)
	}
	for name, body := range map[string]string{"status": statusBody, "claim": claimBody} {
		if !strings.Contains(body, "req_source") {
			t.Fatalf("%s body = %q, want {\"req_source\":1}", name, body)
		}
	}
}

// TestCheckin9095IsIdempotentSuccess: "this device has checked in today" is
// the day's success, not a failure — no retries burned, no red task row.
func TestCheckin9095IsIdempotentSuccess(t *testing.T) {
	var claims int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			fmt.Fprint(w, `{"checked_in":false,"enable":true,"credits":0}`)
			return
		}
		claims++
		fmt.Fprint(w, `{"code":9095,"message":"today already checked in"}`)
	}))
	defer srv.Close()
	oldDelay := checkinRetryDelay
	checkinRetryDelay = time.Millisecond
	t.Cleanup(func() { checkinRetryDelay = oldDelay })

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1", "device_id": "1234567890123456"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	checkedIn, _, _, err := d.Checkin(context.Background(), &model.Account{ID: "trae:u1", CredsJSON: creds})
	if err != nil {
		t.Fatalf("9095 must not be an error: %v", err)
	}
	if !checkedIn {
		t.Fatal("9095 must read as checked-in")
	}
	if claims != 1 {
		t.Fatalf("9095 must not retry, claims=%d", claims)
	}
}

// TestCheckin9074FailsFast: "go check in via the app" is not retryable —
// three 20s waits per account per slot is the current waste.
func TestCheckin9074FailsFast(t *testing.T) {
	var claims int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			fmt.Fprint(w, `{"checked_in":false,"enable":true,"credits":0}`)
			return
		}
		claims++
		fmt.Fprint(w, `{"code":9074,"message":"请前往App参与活动"}`)
	}))
	defer srv.Close()
	oldDelay := checkinRetryDelay
	checkinRetryDelay = time.Millisecond
	t.Cleanup(func() { checkinRetryDelay = oldDelay })

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1", "device_id": "1234567890123456"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	_, _, _, err := d.Checkin(context.Background(), &model.Account{ID: "trae:u1", CredsJSON: creds})
	if err == nil {
		t.Fatal("9074 must surface as a failure")
	}
	if claims != 1 {
		t.Fatalf("9074 must fail on the first claim, claims=%d", claims)
	}
	ue, ok := uperr.As(err)
	if !ok || !strings.Contains(ue.Message, "9074") {
		t.Fatalf("error must carry the business code for the panel, got %v", err)
	}
}

// TestCheckinStatus1001IsAuth: a 200 status body with code=1001 (dead JWT)
// must surface as ClassAuth — not as "not checked in, carry on" — so the
// task ledger records a failure and the account's dead credential becomes
// visible.
func TestCheckinStatus1001IsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":1001,"message":"not able to authenticate","checked_in":false,"enable":false}`)
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1", "device_id": "1234567890123456"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	_, _, _, err := d.Checkin(context.Background(), &model.Account{ID: "trae:u1", CredsJSON: creds})
	if err == nil {
		t.Fatal("code=1001 in the status body must be an error")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassAuth {
		t.Fatalf("status 1001 class = %v, want auth", err)
	}
}

// TestCheckinRetryOnlyOnTransport: a transport failure is the only case that
// earns the retry loop.
func TestCheckinRetryOnlyOnTransport(t *testing.T) {
	var claims int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			fmt.Fprint(w, `{"checked_in":false,"enable":true,"credits":0}`)
			return
		}
		claims++
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"upstream hiccup"}`)
	}))
	defer srv.Close()
	oldDelay := checkinRetryDelay
	checkinRetryDelay = time.Millisecond
	t.Cleanup(func() { checkinRetryDelay = oldDelay })

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1", "device_id": "1234567890123456"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	_, _, _, err := d.Checkin(context.Background(), &model.Account{ID: "trae:u1", CredsJSON: creds})
	if err == nil {
		t.Fatal("502 must surface as a failure")
	}
	if claims != 3 {
		t.Fatalf("transport failure must use the retry loop, claims=%d", claims)
	}
}

// TestCheckinConcurrentSingleFlight (TRAE-2 safety): the device normalization
// is pure derivation — two goroutines on the same account must derive the
// same id (no shared mutable state to race).
func TestCheckinConcurrentSingleFlight(t *testing.T) {
	const uid = "uid-conc"
	var wg sync.WaitGroup
	seen := make(map[string]bool)
	var mu sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := ugDeviceID("", uid)
			mu.Lock()
			seen[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Fatalf("derivation must be deterministic under concurrency, saw %d distinct ids", len(seen))
	}
}
