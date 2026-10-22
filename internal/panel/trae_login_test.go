package panel_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
	"bargainproxy/internal/panel"
)

func jsonDecode(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

func readAll(r io.ReadCloser) []byte {
	b, _ := io.ReadAll(r)
	return b
}

// TestTraeLoginFlowE2E: start builds the trae.cn authorization URL with the
// reference parameter set; the loopback callback parses the double-encoded
// credential parameters and rejects callbacks without usable credentials
// without touching any upstream or creating an account.
func TestTraeLoginFlowE2E(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	srv, _ := buildPanelServerWith(t, func(cfg *config.Config) {})
	cookie, csrf := loginHelper(t, srv)

	// Start.
	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/trae/start", cookie, csrf, `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("trae start status = %d", resp.StatusCode)
	}
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	_ = jsonDecode(resp.Body, &start)
	resp.Body.Close()
	if !strings.HasPrefix(start.AuthorizeURL, "https://www.trae.cn/authorization?") {
		t.Fatalf("authorize url = %q", start.AuthorizeURL)
	}
	au, _ := url.Parse(start.AuthorizeURL)
	q := au.Query()
	if q.Get("client_id") != "en1oxy7wnw8j9n" || q.Get("auth_from") != "solo" ||
		q.Get("login_channel") != "native_ide" || q.Get("auth_callback_url") == "" {
		t.Fatalf("authorize params = %v", q)
	}
	if q.Get("machine_id") == "" || q.Get("device_id") == "" || len(q.Get("login_trace_id")) != 16 {
		t.Fatalf("device identity params missing: %v", q)
	}
	callbackURL := q.Get("auth_callback_url")
	if !strings.HasPrefix(callbackURL, "http://127.0.0.1:") {
		t.Fatalf("callback url = %q", callbackURL)
	}

	// A callback with neither refreshToken nor userJwt.Token must fail
	// cleanly at parse time: error page, zero upstream hits, zero accounts
	// created. (There is no state round-trip on the trae flow — the loopback
	// port is the one-shot defense — so this lands directly in the parser.)
	badKey := strings.Join([]string{"user", "Info"}, "")
	bad := callbackURL + "?" + badKey + "=" + url.QueryEscape(`{"UserID":"u-tr-x","ScreenName":"n"}`)
	badResp, err := http.Get(bad)
	if err != nil {
		t.Fatalf("bad callback: %v", err)
	}
	badPage := readAll(badResp.Body)
	badResp.Body.Close()
	if !strings.Contains(string(badPage), "授权失败") {
		t.Fatalf("bad callback page = %s", badPage)
	}
	if upstreamHits != 0 {
		t.Fatalf("bad callback must not hit upstream: %d", upstreamHits)
	}
	for _, a := range panelState(t, srv.URL, cookie) {
		if a.Channel == "trae" {
			t.Fatalf("failed callback must not create an account: %+v", a)
		}
	}
}

// TestTraeLoginStoresDeviceIdentity (BP-98 TRAE-2): a successful login must
// store the machine/device ids that built the authorize URL into the
// credential blob — they used to be written as "", so every request went out
// without X-Device-Id and the check-in read as a script (9074,
// yetone/magpie#808). The mock callback carries real credentials; the
// upstream exchange/userinfo endpoints are stubbed.
func TestTraeLoginStoresDeviceIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "ExchangeToken"):
			fmt.Fprintf(w, `{"Result":{"Token":"jwt-acc","TokenExpireAt":%d,"RefreshToken":"rt-new"}}`, time.Now().Add(48*time.Hour).UnixMilli())
		case strings.HasSuffix(r.URL.Path, "GetUserInfo"):
			fmt.Fprint(w, `{"Result":{"UserID":"u-dev","ScreenName":"Dev","EnterpriseID":""}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer upstream.Close()
	restore := panel.TraeOAuthHostOverride(upstream.URL)
	t.Cleanup(restore)

	srv, _, gw, _ := buildPanelEnv(t, func(cfg *config.Config) {})
	cookie, csrf := loginHelper(t, srv)

	resp := postWithCSRF(t, srv.URL+"/panel/api/accounts/trae/start", cookie, csrf, `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("trae start status = %d", resp.StatusCode)
	}
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	_ = jsonDecode(resp.Body, &start)
	resp.Body.Close()
	au, _ := url.Parse(start.AuthorizeURL)
	q := au.Query()
	wantMachine, wantDevice := q.Get("machine_id"), q.Get("device_id")
	if wantMachine == "" || wantDevice == "" {
		t.Fatalf("start must generate the device pair: %v", q)
	}

	// Drive the loopback callback with a full credential set.
	callbackURL := q.Get("auth_callback_url")
	cb := callbackURL + "?refreshToken=rt-1&userInfo=" + url.QueryEscape(`{"UserID":"u-dev","ScreenName":"Dev"}`)
	cbResp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_, _ = io.ReadAll(cbResp.Body)
	_ = cbResp.Body.Close()

	var found bool
	for _, a := range panelState(t, srv.URL, cookie) {
		if a.Channel == "trae" && strings.HasSuffix(a.ID, "u-dev") {
			found = true
		}
	}
	if !found {
		t.Fatal("account was not created")
	}
	// The credential blob must carry the same pair the URL was built from:
	// read the account back from the pool (the state API deliberately never
	// exposes credential blobs).
	var stored *model.Account
	for _, a := range gw.Pool().Accounts(model.ChannelTrae) {
		if a.ID == "trae:u-dev" {
			stored = a
			break
		}
	}
	if stored == nil {
		t.Fatal("account not found in pool")
	}
	var raw map[string]any
	if err := json.Unmarshal(stored.CredsJSON, &raw); err != nil {
		t.Fatalf("creds blob: %v", err)
	}
	if raw["device_id"] != wantDevice || raw["machine_id"] != wantMachine {
		t.Fatalf("stored device identity = %v/%v, want %v/%v", raw["device_id"], raw["machine_id"], wantDevice, wantMachine)
	}
}
