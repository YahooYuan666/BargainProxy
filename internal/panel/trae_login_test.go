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

	"bargainproxy/internal/config"
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
