package trae

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// BP-280 (magpie-community/plugins trae 0.2.29, #709/#722): a 9074 with no
// local Trae CN client gets ONE more claim in the shape three no-client tools
// send (luckymiaow/trae-mate MIT, dsh-providers-hub, wild-work) — body {},
// x-user-region CN, a VSCode (not Electron) User-Agent, 15-digit device id
// and market/session ids derived from the uid alone. magpie's own claim gets
// 9074 on accounts those tools check in daily, and none of them retries a
// 9074: waiting is not what changes it, the shape is. Which field Trae CN
// wants has not been measured, so the request goes whole, and only once — a
// second 9074 is the day's answer. The account's own device identity (BP-98/
// BP-210) is never rotated: 9095 on it stays the day's success.

type claimShapeMock struct {
	landed          bool // a successful client-shape claim registers the day
	afterCreds      int64
	normal9074      bool
	shape9074       bool
	normalClaims    int
	shapeClaims     int
	shapeUA         string
	shapeDevice     string
	shapeMarketUser string
	shapeSession    string
}

func (m *claimShapeMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trae/api/v2/ug/checkin_credits/status":
			fmt.Fprintf(w, `{"checked_in":%v,"credits":%d,"enable":true}`, m.landed, m.afterCreds)
		case "/trae/api/v2/ug/checkin_credits/claim":
			raw, _ := io.ReadAll(r.Body)
			if string(raw) == "{}" {
				m.shapeClaims++
				m.shapeUA = r.Header.Get("User-Agent")
				m.shapeDevice = r.Header.Get("X-Device-Id")
				m.shapeMarketUser = r.Header.Get("X-Market-User-Id")
				m.shapeSession = r.Header.Get("Vscode-Sessionid")
				if m.shape9074 {
					fmt.Fprint(w, `{"code":9074,"message":"check in via the app"}`)
					return
				}
				m.landed = true
				fmt.Fprint(w, `{"code":0}`)
				return
			}
			m.normalClaims++
			if m.normal9074 {
				fmt.Fprint(w, `{"code":9074,"message":"check in via the app"}`)
				return
			}
			fmt.Fprint(w, `{"code":0}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestBP280Ninety074GetsExactlyOneClientShapeClaim(t *testing.T) {
	m := &claimShapeMock{normal9074: true, afterCreds: 300}
	srv := m.server(t)
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	ok, credits, enable, err := d.Checkin(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !ok || !enable || credits != 300 {
		t.Fatalf("checkedIn=%v enable=%v credits=%d — the client-shape claim is what lands the day", ok, enable, credits)
	}
	if m.normalClaims != 1 || m.shapeClaims != 1 {
		t.Fatalf("claims = normal %d shape %d, want exactly 1 + 1 (one retry, never more)", m.normalClaims, m.shapeClaims)
	}
}

func TestBP280Second9074IsTheDaysAnswer(t *testing.T) {
	m := &claimShapeMock{normal9074: true, shape9074: true}
	srv := m.server(t)
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	_, _, _, err := d.Checkin(context.Background(), testAccount())
	if err == nil {
		t.Fatal("a 9074 the client shape cannot lift must surface as the day's answer")
	}
	if !strings.Contains(err.Error(), "9074") {
		t.Fatalf("the 9074 must ride through, got: %v", err)
	}
	if m.shapeClaims != 1 || m.normalClaims != 1 {
		t.Fatalf("claims = normal %d shape %d, want 1 + 1 (no retries after the shape)", m.normalClaims, m.shapeClaims)
	}
}

func TestBP280Non9074NeverTouchesTheClientShape(t *testing.T) {
	// 9095 "this device has checked in today" is the day's success on the
	// account's OWN device — no client-shape request may ride with it.
	m := &claimShapeMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trae/api/v2/ug/checkin_credits/status":
			fmt.Fprint(w, `{"checked_in":false,"credits":100,"enable":true}`)
		case "/trae/api/v2/ug/checkin_credits/claim":
			m.normalClaims++
			fmt.Fprint(w, `{"code":9095,"message":"this device has checked in today"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	ok, _, _, err := d.Checkin(context.Background(), testAccount())
	if err != nil || !ok {
		t.Fatalf("9095 is the day's success: err=%v ok=%v", err, ok)
	}
	if m.shapeClaims != 0 {
		t.Fatalf("client-shape claims = %d, want 0 for a non-9074 answer", m.shapeClaims)
	}
}

func TestBP280ClientShapeHeadersAreTraeMateShape(t *testing.T) {
	m := &claimShapeMock{normal9074: true, afterCreds: 100}
	srv := m.server(t)
	defer srv.Close()

	d := testDriver(srv.URL, srv.URL)
	_, _, _, _ = d.Checkin(context.Background(), testAccount())

	if m.shapeUA != "VSCode 1.107.1 (TRAE SOLO CN)" {
		t.Fatalf("shape UA = %q, want the VSCode shell (no Electron)", m.shapeUA)
	}
	if !regexp.MustCompile(`^\d{15}$`).MatchString(m.shapeDevice) {
		t.Fatalf("shape device id = %q, want 15 digits derived from the uid", m.shapeDevice)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(m.shapeMarketUser) {
		t.Fatalf("shape market user = %q, want a UUID (v4 shape) derived from the uid", m.shapeMarketUser)
	}
	if len(m.shapeSession) != 64 {
		t.Fatalf("vscode-sessionid length = %d, want 64 hex chars", len(m.shapeSession))
	}
	// Derivation determinism: the same uid always maps to the same ids.
	if traeMateDeviceID("u-7") != traeMateDeviceID("u-7") || traeMateMarketUserID("u-7") != traeMateMarketUserID("u-7") {
		t.Fatal("uid-derived ids must be deterministic")
	}
	if traeMateDeviceID("u-7") == traeMateDeviceID("u-8") {
		t.Fatal("different uids must not share ids")
	}
}
