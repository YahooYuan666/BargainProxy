// BP-88: the update notice must be honest, read-only and impossible to turn
// into a download/replace path by accident. These tests lock the API contract
// the panel JS consumes, plus the failure modes that would otherwise reach the
// operator as a false "new version" banner.
package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/store"
	"bargainproxy/internal/version"
)

// Feed fixture: the same real capture the version package uses.
const (
	feedFixturePath = "../version/testdata/commits.atom"
	headCommit      = "a975d93f959ac0016dc287c6a473bec925892def"
	olderCommit     = "d91727e2356af42030477567e8f72ffb94f1221a"
)

// Stub status strings, mirrored from the version package so the assertions
// spell out exactly what the panel JS switches on.
const (
	versionStatusUnknown = version.StatusUnknown
	versionStatusLatest  = version.StatusLatest
	versionStatusBehind  = version.StatusBehind
	versionStatusLocal   = version.StatusLocal
	versionStatusError   = version.StatusError
)

// readFile reads a test fixture; a missing file must fail the test loudly.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return b
}

func stubFeed(t *testing.T) *httptest.Server {
	t.Helper()
	body := readFile(t, feedFixturePath)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checkerFor(t *testing.T, rev string, modified bool, feedURL string) *version.Checker {
	t.Helper()
	c := version.NewCheckerAt(&http.Client{Timeout: 5 * time.Second}, feedURL)
	c.SetBuild(version.Build{Revision: rev, Modified: modified, Known: rev != ""})
	return c
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", string(body), err)
	}
	return out
}

func postVersion(t *testing.T, p *Panel, path string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	switch path {
	case "/panel/api/version/check":
		p.handleVersionCheck(rec, req)
	case "/panel/api/version/auto":
		p.handleUpdateCheckToggle(rec, req)
	default:
		t.Fatalf("unknown path %s", path)
	}
	return rec, decodeJSON(t, rec.Body.Bytes())
}

func TestVersionGetNeverTouchesTheNetwork(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	// A feed URL that cannot be reached: if GET touched it, the status would
	// come back as an error instead of "unknown".
	p.SetVersionChecker(checkerFor(t, olderCommit, false, "http://127.0.0.1:1/never"))

	rec := httptest.NewRecorder()
	p.handleVersionGet(rec, httptest.NewRequest(http.MethodGet, "/panel/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusUnknown {
		t.Fatalf("an unchecked panel must report %q, got %v", versionStatusUnknown, got["status"])
	}
	if got["update_available"] != false {
		t.Fatal("an unchecked panel must never claim an available update")
	}
	if got["auto_check_enabled"] != true {
		t.Fatal("automatic check defaults to on")
	}
}

func TestVersionCheckMarksBehindForCleanBuild(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	p.SetVersionChecker(checkerFor(t, olderCommit, false, stubFeed(t).URL))

	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusBehind || got["update_available"] != true {
		t.Fatalf("a clean older build must report behind: %v", got)
	}
	remote, _ := got["remote"].(map[string]any)
	if remote == nil || remote["short"] != "a975d93" {
		t.Fatalf("remote commit missing: %v", got["remote"])
	}
	if b, _ := got["build"].(map[string]any); b == nil || b["short"] != "d91727e" {
		t.Fatalf("build stamp missing: %v", got["build"])
	}
}

// The case this feature exists for on the operator's own machine: a binary
// built from a dirty tree must not claim an update.
func TestVersionCheckDirtyBuildNeverClaimsUpdate(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	p.SetVersionChecker(checkerFor(t, olderCommit, true, stubFeed(t).URL))

	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusLocal {
		t.Fatalf("dirty build status = %v, want %q", got["status"], versionStatusLocal)
	}
	if got["update_available"] != false {
		t.Fatal("dirty build must not claim an update — the diff may already be in the working tree")
	}
	if s, _ := got["summary"].(string); !strings.Contains(s, "无法判断") {
		t.Fatalf("summary must say the comparison is impossible: %v", got["summary"])
	}
}

func TestVersionCheckLatestOnSameCommit(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	p.SetVersionChecker(checkerFor(t, headCommit, false, stubFeed(t).URL))

	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusLatest {
		t.Fatalf("status = %v, want %q", got["status"], versionStatusLatest)
	}
	if got["update_available"] != false {
		t.Fatal("being up to date must not claim an update")
	}
	if got["notice"] != false {
		t.Fatal("being up to date must not raise the banner")
	}
}

// The operator's own machine, today: a dirty build whose base commit happens
// to be the current remote head. Nothing moved upstream, so the banner must
// stay silent — showing it would be pure noise (found by running the real
// binary against the real feed, not by reasoning).
func TestVersionCheckDirtyBuildOnHeadStaysSilent(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	p.SetVersionChecker(checkerFor(t, headCommit, true, stubFeed(t).URL))

	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusLocal {
		t.Fatalf("status = %v, want %q", got["status"], versionStatusLocal)
	}
	if got["notice"] != false {
		t.Fatal("上游没有比基准更新的提交时不得弹提醒条")
	}
	if got["update_available"] != false {
		t.Fatal("dirty build must never claim an update")
	}
}

// A dirty build whose base commit is behind the remote *does* raise a notice
// (something moved), while still refusing to call it an update.
func TestVersionCheckDirtyBuildBehindRaisesNoticeButNotUpdate(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	p.SetVersionChecker(checkerFor(t, olderCommit, true, stubFeed(t).URL))

	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusLocal || got["notice"] != true {
		t.Fatalf("something moved upstream: status=%v notice=%v", got["status"], got["notice"])
	}
	if got["update_available"] != false {
		t.Fatal("still must not claim an update — the diff may already be local")
	}
}

func TestVersionCheckNetworkFailureIsReportedNotFatal(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	p.SetVersionChecker(checkerFor(t, olderCommit, false, "http://127.0.0.1:1/unreachable"))

	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("a network failure must still answer 200 with an error state, got %d", rec.Code)
	}
	got := decodeJSON(t, rec.Body.Bytes())
	if got["status"] != versionStatusError {
		t.Fatalf("status = %v, want %q", got["status"], versionStatusError)
	}
	if got["update_available"] != false {
		t.Fatal("a failed check must never claim an update")
	}
	if s, _ := got["last_error"].(string); s == "" {
		t.Fatal("the failure reason must be surfaced for troubleshooting")
	}
}

func TestVersionAutoTogglePersists(t *testing.T) {
	st := store.NewMemory()
	p := &Panel{store: st}

	rec, got := postVersion(t, p, "/panel/api/version/auto", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got["auto_check_enabled"] != false {
		t.Fatalf("response must echo the new preference: %v", got)
	}
	if p.UpdateCheckEnabled() {
		t.Fatal("switching off must apply in-process")
	}
	if v, ok, _ := st.GetSetting(KeyUpdateCheckEnabled); !ok || v != "false" {
		t.Fatalf("store = %q ok=%v", v, ok)
	}
	// A fresh panel over the same store must see the persisted value.
	if (&Panel{store: st}).UpdateCheckEnabled() {
		t.Fatal("preference must survive a panel rebuild")
	}
}

func TestVersionAutoToggleRejectsMissingField(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	rec, _ := postVersion(t, p, "/panel/api/version/auto", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestVersionStateWithoutCheckerDegrades(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	rec := httptest.NewRecorder()
	p.handleVersionGet(rec, httptest.NewRequest(http.MethodGet, "/panel/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeJSON(t, rec.Body.Bytes())
	if got["update_available"] != false {
		t.Fatal("no checker must not imply an update")
	}
}

func TestVersionCheckWithoutCheckerIs503(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	rec := httptest.NewRecorder()
	p.handleVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/version/check", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// The background loop must exit immediately when there is no checker (a
// gateway without one must not leak a goroutine).
func TestRunUpdateChecksWithoutCheckerReturns(t *testing.T) {
	p := &Panel{store: store.NewMemory()}
	done := make(chan struct{})
	go func() { p.RunUpdateChecks(nil, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunUpdateChecks must return immediately without a checker")
	}
}

// Disabling the switch must stop the automatic loop from fetching, while the
// panel's explicit button keeps working (an explicit click is not a poll).
func TestRunUpdateChecksHonoursTheSwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			st := store.NewMemory()
			if err := st.SetSetting(KeyUpdateCheckEnabled, boolStr(enabled)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				_, _ = w.Write(readFile(t, feedFixturePath))
			}))
			defer srv.Close()

			p := &Panel{store: st, startDelay: time.Millisecond}
			p.SetVersionChecker(checkerFor(t, olderCommit, false, srv.URL))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { p.RunUpdateChecks(ctx, time.Hour); close(done) }()
			// Give the loop time to do its single fetch (or prove it does not).
			waitFor(t, 2*time.Second, func() bool {
				if enabled {
					return atomic.LoadInt32(&hits) >= 1
				}
				return false
			})
			cancel()
			<-done

			n := atomic.LoadInt32(&hits)
			if enabled && n != 1 {
				t.Fatalf("enabled auto-check fetched %d times, want 1", n)
			}
			if !enabled && n != 0 {
				t.Fatalf("disabled auto-check fetched %d times, want 0", n)
			}
		})
	}
}

// waitFor polls cond until it is true or the deadline passes. It exists so the
// loop tests do not encode a fixed sleep as an implicit assumption.
func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
