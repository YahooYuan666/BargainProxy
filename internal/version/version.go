// Package version answers one question for the panel: "is the source on
// GitHub newer than the code this binary was built from?" (BP-88).
//
// It is deliberately read-only and side-effect free: it never downloads a
// release, never replaces the executable and never restarts the gateway. The
// repository ships source only, so there is no release artifact to fetch —
// all this package can honestly offer is a notice that the source moved.
//
// The central honesty problem: a binary built from a dirty working tree
// recorded its *base* commit, not the code actually inside it. Comparing the
// base commit with the remote head would then report "new version" for
// commits the developer already has locally. Those builds therefore report
// StatusLocal and never claim an update is available.
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
	"time"
)

// Repo is the upstream project the notice points at.
const (
	Repo   = "YahooYuan666/BargainProxy"
	Branch = "main"
)

// Status values are stable strings: the panel JS switches on them and the
// tests lock their spelling.
const (
	// StatusLatest: clean build, remote head == the built revision.
	StatusLatest = "latest"
	// StatusBehind: clean build, remote head != the built revision.
	StatusBehind = "behind"
	// StatusLocal: the build carried uncommitted changes, so its base commit
	// does not describe the code inside it —落后与否无从判断.
	StatusLocal = "local_build"
	// StatusNoVCS: the binary carries no VCS stamp at all (built without git,
	// or via `go run`).
	StatusNoVCS = "no_build_info"
	// StatusError: the remote could not be read (offline, blocked, rate limit).
	StatusError = "error"
	// StatusUnknown: no check has completed yet.
	StatusUnknown = "unknown"
)

// Build describes what this binary knows about its own origin.
type Build struct {
	Revision string    `json:"revision"` // full VCS revision, empty when unknown
	Time     time.Time `json:"time"`     // commit time of Revision (not build time)
	Modified bool      `json:"modified"` // built from a dirty working tree
	Known    bool      `json:"known"`    // a revision was recorded at all
}

// Short returns the 7-character revision, or "—" when unknown.
func (b Build) Short() string { return shortSHA(b.Revision) }

// ReadBuild reads the Go build stamp of the running process.
func ReadBuild() Build {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return Build{}
	}
	return buildFromSettings(bi.Settings)
}

// buildFromSettings is the testable half of ReadBuild: it turns the raw
// -ldflags/-X style settings list into a Build. Split out so tests can supply
// each shape (clean / dirty / absent) without a specially built binary.
func buildFromSettings(settings []debug.BuildSetting) Build {
	var b Build
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				b.Time = t
			}
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	b.Known = b.Revision != ""
	return b
}

// Commit is the remote head commit.
type Commit struct {
	SHA   string    `json:"sha"`
	Title string    `json:"title"`
	Time  time.Time `json:"time"`
	URL   string    `json:"url"`
}

// Short returns the 7-character SHA.
func (c Commit) Short() string { return shortSHA(c.SHA) }

// Verdict is the outcome of comparing a build with the remote head.
type Verdict struct {
	Status string
	// Summary is the one-line sentence the panel shows. It lives here (not in
	// the JS) so the wording is testable.
	Summary string
	// UpdateAvailable is true only for StatusBehind. Local builds and unknown
	// states must never set it — that is the whole point of the distinction.
	UpdateAvailable bool
	// RemoteDiffersFromBase reports that the remote head differs from the base
	// commit of a dirty build. It is informational only: the difference may
	// already be present in the uncommitted changes.
	RemoteDiffersFromBase bool
}

// Evaluate compares a build against the remote head commit.
func Evaluate(b Build, r Commit) Verdict {
	switch {
	case !b.Known:
		return Verdict{
			Status:  StatusNoVCS,
			Summary: "这个程序没有记录构建来源提交（不是在 git 仓库里编译的），无法与 GitHub 比较。",
		}
	case b.Modified:
		if sameRevision(b.Revision, r.SHA) {
			return Verdict{
				Status:  StatusLocal,
				Summary: fmt.Sprintf("本机构建（带未提交改动）。上游最新仍是 %s，没有比你编译基准更新的提交。", r.Short()),
			}
		}
		return Verdict{
			Status:                StatusLocal,
			Summary:               fmt.Sprintf("本机构建（带未提交改动），无法判断是否落后。上游最新是 %s（%s）：%s——提醒：你可能已经把这些改动做在本地了。", r.Short(), fmtCommitTime(r.Time), r.Title),
			RemoteDiffersFromBase: true,
		}
	case sameRevision(b.Revision, r.SHA):
		return Verdict{
			Status:  StatusLatest,
			Summary: fmt.Sprintf("已是最新提交 %s。", r.Short()),
		}
	default:
		return Verdict{
			Status:          StatusBehind,
			Summary:         fmt.Sprintf("GitHub 上有新提交 %s（%s）：%s", r.Short(), fmtCommitTime(r.Time), r.Title),
			UpdateAvailable: true,
		}
	}
}

// sameRevision compares two revisions leniently: the build stamp always holds
// the full 40-character SHA while a feed or a hand-typed value may be the
// 7-character short form.
func sameRevision(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	// a is now the longer one; a short b is a prefix match.
	return len(b) >= 7 && strings.HasPrefix(a, b)
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 7 {
		return s[:7]
	}
	if s == "" {
		return "—"
	}
	return s
}

func fmtCommitTime(t time.Time) string {
	if t.IsZero() {
		return "时间未知"
	}
	return t.Local().Format("2006-01-02 15:04")
}
