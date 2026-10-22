package version

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// realHead is the newest commit in testdata/commits.atom (captured from the
// live feed on 2026-10-22).
const (
	realHead      = "a975d93f959ac0016dc287c6a473bec925892def"
	realHeadShort = "a975d93"
)

func TestReadBuildFromSettings(t *testing.T) {
	// debug.BuildSetting is a plain struct, so the three shapes the panel must
	// distinguish can be simulated exactly.
	set := func(kv ...string) []debug.BuildSetting {
		var out []debug.BuildSetting
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return out
	}
	cases := []struct {
		name     string
		settings []debug.BuildSetting
		want     Build
	}{
		{
			name:     "clean build records revision and time",
			settings: set("vcs.revision", "abc123def456", "vcs.time", "2026-10-21T17:16:42Z", "vcs.modified", "false"),
			want:     Build{Revision: "abc123def456", Time: time.Date(2026, 10, 21, 17, 16, 42, 0, time.UTC), Known: true},
		},
		{
			name:     "dirty build is flagged modified",
			settings: set("vcs.revision", "d91727e2356a", "vcs.time", "2026-10-19T10:04:51Z", "vcs.modified", "true"),
			want:     Build{Revision: "d91727e2356a", Time: time.Date(2026, 10, 19, 10, 4, 51, 0, time.UTC), Modified: true, Known: true},
		},
		{
			name:     "no vcs settings means unknown",
			settings: set("GOOS", "windows", "-buildmode", "exe"),
			want:     Build{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFromSettings(tc.settings)
			if got.Revision != tc.want.Revision || !got.Time.Equal(tc.want.Time) ||
				got.Modified != tc.want.Modified || got.Known != tc.want.Known {
				t.Fatalf("buildFromSettings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReadBuildWithoutInfo(t *testing.T) {
	// A binary with no VCS stamp: buildFromSettings over an empty list. The
	// panel must treat this as "cannot compare", never as "up to date".
	if got := buildFromSettings(nil); got.Known {
		t.Fatalf("missing build info must not be Known: %+v", got)
	}
}

func TestEvaluateStatuses(t *testing.T) {
	remote := Commit{SHA: realHead, Title: "BP-77~86：错误分类", Time: time.Date(2026, 10, 21, 17, 16, 42, 0, time.UTC)}
	cases := []struct {
		name        string
		build       Build
		remote      Commit
		wantStatus  string
		wantUpdate  bool
		wantDiffers bool
	}{
		{
			name:       "clean build on the head commit is latest",
			build:      Build{Revision: realHead, Known: true},
			remote:     remote,
			wantStatus: StatusLatest,
		},
		{
			name:       "clean build on an older commit is behind",
			build:      Build{Revision: "d91727e2356af42030477567e8f72ffb94f1221a", Known: true},
			remote:     remote,
			wantStatus: StatusBehind,
			wantUpdate: true,
		},
		{
			name:       "short revision still matches the full sha",
			build:      Build{Revision: realHeadShort, Known: true},
			remote:     remote,
			wantStatus: StatusLatest,
		},
		{
			// The decisive case: a dirty build must NEVER claim an update,
			// even when the remote head differs from its base commit.
			name:        "dirty build with a differing remote is local, not behind",
			build:       Build{Revision: "d91727e2356af42030477567e8f72ffb94f1221a", Modified: true, Known: true},
			remote:      remote,
			wantStatus:  StatusLocal,
			wantUpdate:  false,
			wantDiffers: true,
		},
		{
			name:       "dirty build on the head commit has nothing newer upstream",
			build:      Build{Revision: realHead, Modified: true, Known: true},
			remote:     remote,
			wantStatus: StatusLocal,
		},
		{
			name:       "binary without a vcs stamp cannot be compared",
			build:      Build{},
			remote:     remote,
			wantStatus: StatusNoVCS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.build, tc.remote)
			if v.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (summary: %s)", v.Status, tc.wantStatus, v.Summary)
			}
			if v.UpdateAvailable != tc.wantUpdate {
				t.Fatalf("UpdateAvailable = %v, want %v — 谎报更新是最坏的失败模式", v.UpdateAvailable, tc.wantUpdate)
			}
			if v.RemoteDiffersFromBase != tc.wantDiffers {
				t.Fatalf("RemoteDiffersFromBase = %v, want %v", v.RemoteDiffersFromBase, tc.wantDiffers)
			}
			if strings.TrimSpace(v.Summary) == "" {
				t.Fatal("every verdict must carry a human sentence")
			}
		})
	}
}

func TestParseRealFeed(t *testing.T) {
	body, err := os.ReadFile("testdata/commits.atom")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := ParseFeed(body)
	if err != nil {
		t.Fatalf("ParseFeed: %v", err)
	}
	if c.SHA != realHead {
		t.Fatalf("sha = %q, want %q", c.SHA, realHead)
	}
	if c.Short() != realHeadShort {
		t.Fatalf("short = %q, want %q", c.Short(), realHeadShort)
	}
	if c.Time.IsZero() {
		t.Fatal("commit time must parse")
	}
	if c.Title == "" || strings.Contains(c.Title, "\n") {
		t.Fatalf("title must be a single non-empty line: %q", c.Title)
	}
	if !strings.Contains(c.URL, "/commit/") {
		t.Fatalf("url = %q", c.URL)
	}
}

func TestParseFeedRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"not xml":      "<feed><entry>",
		"no entries":   `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`,
		"no commit id": `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><id>tag:github.com,2008:/x</id><title>t</title></entry></feed>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFeed([]byte(body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestShaFromCommitID(t *testing.T) {
	cases := map[string]string{
		"tag:github.com,2008:Grit::Commit/a975d93f959ac0016dc287c6a473bec925892def": "a975d93f959ac0016dc287c6a473bec925892def",
		"tag:github.com,2008:/x":            "",
		"":                                  "",
		"tag:github.com,2008:Grit::Commit/": "",
		"tag:github.com,2008:Grit::Commit/zzzzzzzz": "",
	}
	for in, want := range cases {
		if got := shaFromCommitID(in); got != want {
			t.Fatalf("shaFromCommitID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckerRecordsErrorWithoutLying(t *testing.T) {
	c := NewChecker(nil)
	c.build = Build{Revision: realHead, Known: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("GitHub rejects requests without a User-Agent")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c.hc = &http.Client{Timeout: time.Second}
	c.feedOverride = srv.URL

	st := c.Check(context.Background(), 0)
	if st.Status != StatusError {
		t.Fatalf("status = %q, want %q", st.Status, StatusError)
	}
	if st.UpdateAvailable {
		t.Fatal("a failed check must never report an available update")
	}
	if st.LastError == "" {
		t.Fatal("the failure reason must be kept for the panel")
	}
	if st.CheckedAt.IsZero() {
		t.Fatal("checked_at must be stamped even on failure")
	}
}

func TestCheckerSuccessIsCachedAndThrottled(t *testing.T) {
	body, err := os.ReadFile("testdata/commits.atom")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewChecker(nil)
	c.build = Build{Revision: "d91727e2356af42030477567e8f72ffb94f1221a", Known: true}
	c.hc = &http.Client{Timeout: time.Second}
	c.feedOverride = srv.URL

	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	st := c.Check(context.Background(), MinManualInterval)
	if st.Status != StatusBehind || !st.UpdateAvailable {
		t.Fatalf("first check: status=%q update=%v", st.Status, st.UpdateAvailable)
	}
	if st.Remote == nil || st.Remote.SHA != realHead {
		t.Fatalf("remote not recorded: %+v", st.Remote)
	}

	// Inside the throttle window: no second request.
	now = now.Add(5 * time.Second)
	if got := c.Check(context.Background(), MinManualInterval); got.Status != StatusBehind {
		t.Fatalf("throttled check must reuse the cached verdict, got %q", got.Status)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("throttle leaked %d requests, want 1", n)
	}

	// Past the window: a real refetch happens.
	now = now.Add(MinManualInterval + time.Second)
	c.Check(context.Background(), MinManualInterval)
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("expected a refetch after the window, got %d requests", n)
	}
}

func TestStateDoesNotTouchTheNetwork(t *testing.T) {
	c := NewChecker(nil)
	c.hc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("State() must never perform a request")
		return nil, errors.New("unreachable")
	})}
	st := c.State()
	if st.Status != StatusUnknown {
		t.Fatalf("fresh checker status = %q, want %q", st.Status, StatusUnknown)
	}
	if st.Repo == "" || st.RepoURL == "" {
		t.Fatal("state must carry the repo for the panel link")
	}
}

func TestCheckerSurvivesTransportFailure(t *testing.T) {
	c := NewChecker(nil)
	c.build = Build{Revision: realHead, Known: true}
	c.hc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}
	st := c.Check(context.Background(), 0)
	if st.Status != StatusError || st.UpdateAvailable {
		t.Fatalf("offline check = %+v", st)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
