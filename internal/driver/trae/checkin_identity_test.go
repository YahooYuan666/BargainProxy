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
// {req_source: 1} instead of {}. Since 2026-11-05 the claim's device id
// itself must be 15 DIGITS (BP-288, plugins trae 0.2.30 #709 bisect: the
// account's own 16-digit id is answered 9074 where a 15-digit one is
// taken, seconds apart).

// TestUgHeadersCarryDesktopIdentity: ugHeaders sends the Electron UA and a
// check-in device id in the 15-digit shape, mapped from the stored one.
func TestUgHeadersCarryDesktopIdentity(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://x/trae/api/v2/ug/checkin_credits/claim", nil)
	ugHeaders(req, &Creds{AccessToken: "jwt", DeviceID: "1234567890123456"})
	if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "TRAE-SOLO-CN/") || !strings.Contains(ua, "Electron/") {
		t.Fatalf("ug UA must be the Electron shell, got %q", ua)
	}
	got := req.Header.Get("X-Device-Id")
	if len(got) != 15 {
		t.Fatalf("X-Device-Id = %q, want the 15-digit shape (16 digits are answered 9074 since 2026-11-05, BP-288)", got)
	}
	if got == "1234567890123456" {
		t.Fatalf("the stored 16-digit id must be mapped, not sent as is: %q", got)
	}
	if req.Header.Get("X-User-Region") != "CN" {
		t.Fatal("X-User-Region must stay CN")
	}
}

// TestUgDeviceNormalizedTo15Digits: a stored 16/19-digit (or empty-with-uid)
// device id is mapped onto a stable 15-digit one, derived per account —
// never a new random device per request (magpie 0.2.30 ugDevice: a 9095 on
// it must stay the day's answer, so it may not rotate). An id already 15
// digits passes through; 16 digits do NOT (BP-288).
func TestUgDeviceNormalizedTo15Digits(t *testing.T) {
	d1 := ugDeviceID("8800000012345678901", "uid-1")
	if len(d1) != 15 {
		t.Fatalf("derived device id = %q, want 15 digits", d1)
	}
	for i, c := range d1 {
		if c < '0' || c > '9' {
			t.Fatalf("derived device id must be digits only, got %q", d1)
		}
		if i == 0 && c == '0' {
			t.Fatalf("derived device id must not start with 0 (it would be 14 digits as a number): %q", d1)
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
	if d1 == ugDeviceID("8800000012345678902", "uid-1") {
		t.Fatal("different devices must derive different device ids")
	}
	// An id already in the accepted shape passes through untouched.
	if got := ugDeviceID("123456789012345", "uid-1"); got != "123456789012345" {
		t.Fatalf("15-digit id must pass through, got %q", got)
	}
	// A 16-digit id is mapped (not passed through): the 0.2.19 passthrough
	// is what 2026-11-05's server-side change turned against us.
	got16 := ugDeviceID("1234567890123456", "uid-1")
	if got16 == "1234567890123456" || len(got16) != 15 {
		t.Fatalf("16-digit id must map onto a stable 15-digit one, got %q", got16)
	}
	if got16 != ugDeviceID("1234567890123456", "uid-1") {
		t.Fatal("the 16->15 mapping must be stable")
	}
}

// TestUgProbeHeadersKeepStoredDevice: the quota READ (ide_user_ent_usage)
// rides the STORED device id verbatim — the 15-digit mapping is measured
// for the claim chain only (BP-288, plugins 0.2.30).
func TestUgProbeHeadersKeepStoredDevice(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://x/trae/api/v2/pay/ide_user_ent_usage", nil)
	ugProbeHeaders(req, &Creds{AccessToken: "jwt", DeviceID: "1234567890123456", UID: "u1"})
	if got := req.Header.Get("X-Device-Id"); got != "1234567890123456" {
		t.Fatalf("probe X-Device-Id = %q, want the stored id untouched", got)
	}
}

// TestUgBodyReqSource: the ug endpoints get {"req_source":1}, not {} —
// every check-in sent with {} came back 9074 even with the IDE's headers
// (yetone/magpie#808).
func TestUgBodyReqSource(t *testing.T) {
	var statusBody, claimBody string
	claimed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 512)
		n, _ := r.Body.Read(raw)
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			statusBody = string(raw[:n])
			// BP-147: the status is read twice (before and after the claim);
			// the second read is what confirms the day registered.
			if claimed {
				fmt.Fprint(w, `{"checked_in":true,"enable":true,"credits":200}`)
				return
			}
			fmt.Fprint(w, `{"checked_in":false,"enable":true,"credits":100}`)
		default:
			claimBody = string(raw[:n])
			claimed = true
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

// TestCheckin9074FailsFast: a PERMANENT 9074 refusal ("go check in via the
// app") is not retryable — three 20s waits per account per slot is pure waste
// (BP-98 TRAE-3). The RUSH wording ("参与用户太多") is a different answer and
// retries — see TestCheckin9074RushRetries in trae_test.go.
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
	if claims != 2 {
		t.Fatalf("9074 gets the account's own claim + ONE client-shape claim, no more (BP-280); claims=%d", claims)
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
