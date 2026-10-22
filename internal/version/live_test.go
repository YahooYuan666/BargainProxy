package version

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveGitHubFeed is opt-in: it is the only test that talks to the real
// GitHub, and CI (or an offline machine) must not depend on it.
//
//	BARGAINPROXY_LIVE_GH=1 go test ./internal/version -run TestLiveGitHubFeed -v
//
// What it proves that the fixtures cannot: the real feed still parses, still
// carries a SHA in the tag URI we extract from, and still answers without a
// token. The parse path is the one thing that breaks silently when GitHub
// changes its markup.
func TestLiveGitHubFeed(t *testing.T) {
	if os.Getenv("BARGAINPROXY_LIVE_GH") == "" {
		t.Skip("set BARGAINPROXY_LIVE_GH=1 to hit the real GitHub feed")
	}
	c := NewChecker(&http.Client{Timeout: RequestTimeout})
	st := c.Check(context.Background(), 0)
	if st.Status == StatusError {
		t.Fatalf("live check failed: %s", st.LastError)
	}
	if st.Remote == nil {
		t.Fatalf("no remote commit parsed; status=%s", st.Status)
	}
	if len(st.Remote.SHA) != 40 {
		t.Fatalf("remote sha looks wrong: %q", st.Remote.SHA)
	}
	if st.Remote.Title == "" || st.Remote.Time.IsZero() {
		t.Fatalf("remote metadata incomplete: %+v", st.Remote)
	}
	t.Logf("live: head=%s time=%s title=%q build=%q status=%s summary=%q",
		st.Remote.Short(), st.Remote.Time.Format(time.RFC3339), st.Remote.Title,
		st.Build.Short(), st.Status, st.Summary)
	// The build stamp of a `go test` binary carries no VCS revision, so the
	// verdict here must be "cannot compare" — never "update available".
	if st.UpdateAvailable {
		t.Fatal("a test binary has no build stamp; it must not claim an update")
	}
	if st.Status != StatusNoVCS && st.Status != StatusLocal && st.Status != StatusLatest && st.Status != StatusBehind {
		t.Fatalf("unexpected status %q", st.Status)
	}
}
