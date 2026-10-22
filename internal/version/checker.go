package version

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// DefaultInterval is how often the background loop asks GitHub for the head
// commit. Six hours is deliberate: source moves in bursts, the answer changes
// nothing inside the running process, and a fast poll on a personal gateway
// only produces noise. The check costs one small GET.
const DefaultInterval = 6 * time.Hour

// MinManualInterval throttles the panel's "check now" button. Without it a
// held-down click (or an impatient operator) would hammer GitHub while
// changing nothing — the answer cannot change between two clicks.
const MinManualInterval = 30 * time.Second

// RequestTimeout bounds a single feed fetch.
const RequestTimeout = 15 * time.Second

// State is the snapshot the panel renders. It is JSON-serialisable as-is.
type State struct {
	Build     Build     `json:"build"`
	Remote    *Commit   `json:"remote,omitempty"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary"`
	CheckedAt time.Time `json:"checked_at"`
	LastError string    `json:"last_error,omitempty"`
	Repo      string    `json:"repo"`
	RepoURL   string    `json:"repo_url"`
	// UpdateAvailable mirrors the verdict; local builds never set it.
	UpdateAvailable bool `json:"update_available"`
	// Notice is what the panel's banner gates on: an update really exists, or a
	// local build whose base commit differs from the remote (something moved,
	// we just cannot say what). Being up to date — including a dirty build
	// whose base commit equals the remote head — is not a notice.
	Notice bool `json:"notice"`
}

// Checker caches the last comparison and serialises concurrent checks.
type Checker struct {
	hc  *http.Client
	now func() time.Time
	// feedOverride replaces FeedURL() in tests; empty means the real feed.
	feedOverride string

	mu        sync.Mutex
	build     Build
	remote    *Commit
	verdict   Verdict
	checkedAt time.Time
	lastErr   string
	inflight  bool
}

// NewChecker builds a checker for the running binary. hc may be nil (the
// default client is used); the gateway passes its own proxy-aware transport so
// the check honours HTTPS_PROXY like every other outbound call.
func NewChecker(hc *http.Client) *Checker {
	c := &Checker{hc: hc, now: time.Now}
	c.build = ReadBuild()
	c.verdict = Verdict{Status: StatusUnknown, Summary: "还没有检查过。"}
	return c
}

// NewCheckerAt builds a checker against an explicit feed URL. Production uses
// NewChecker (which resolves FeedURL() per request); the explicit form exists
// so the whole compare-and-report path can be exercised without touching
// GitHub.
func NewCheckerAt(hc *http.Client, feedURL string) *Checker {
	c := NewChecker(hc)
	c.feedOverride = feedURL
	return c
}

// SetHTTPClient replaces the client (used by tests and by wiring that builds
// the client after the panel).
func (c *Checker) SetHTTPClient(hc *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hc = hc
}

// SetFeedURL points the checker at another feed (tests). Production leaves it
// empty so FeedURL() is used.
func (c *Checker) SetFeedURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.feedOverride = url
}

// SetBuild overrides the self-description. The build stamp is otherwise baked
// into the binary at link time, so tests pin it here instead of requiring a
// specially built test binary.
func (c *Checker) SetBuild(b Build) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.build = b
}

// Build returns what the running binary knows about itself.
func (c *Checker) Build() Build {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.build
}

// State returns the cached snapshot without touching the network. This is what
// the panel's read endpoint calls, so rendering the page never blocks on
// GitHub — which matters when the network is down or proxied badly.
func (c *Checker) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

func (c *Checker) stateLocked() State {
	st := State{
		Build:           c.build,
		Status:          c.verdict.Status,
		Summary:         c.verdict.Summary,
		CheckedAt:       c.checkedAt,
		LastError:       c.lastErr,
		Repo:            Repo,
		RepoURL:         RepoURL(),
		UpdateAvailable: c.verdict.UpdateAvailable,
		Notice:          c.verdict.UpdateAvailable || c.verdict.RemoteDiffersFromBase,
	}
	if c.remote != nil {
		r := *c.remote
		st.Remote = &r
	}
	// A notice means "go look": either a real update, or a local build whose
	// base commit is not the remote head. A dirty build sitting exactly on the
	// remote head has nothing to look at and must stay silent.
	st.Notice = c.verdict.UpdateAvailable || c.verdict.RemoteDiffersFromBase
	return st
}

// Check refreshes the comparison unless the previous attempt is younger than
// minInterval, in which case the cached state is returned untouched. A fetch
// failure is recorded in LastError and reported as StatusError: it never
// panics, never blocks the caller past RequestTimeout, and never invents a
// "behind" answer.
func (c *Checker) Check(ctx context.Context, minInterval time.Duration) State {
	c.mu.Lock()
	if c.inflight {
		// A check is already running; report what we have instead of piling on.
		st := c.stateLocked()
		c.mu.Unlock()
		return st
	}
	if minInterval > 0 && !c.checkedAt.IsZero() && c.now().Sub(c.checkedAt) < minInterval {
		st := c.stateLocked()
		c.mu.Unlock()
		return st
	}
	c.inflight = true
	hc := c.hc
	build := c.build
	url := c.feedOverride
	if url == "" {
		url = FeedURL()
	}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.inflight = false
		c.mu.Unlock()
	}()

	fetchCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	remote, err := FetchHeadFrom(fetchCtx, hc, url)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = c.now()
	if err != nil {
		c.lastErr = err.Error()
		// Keep any previous verdict's *status* only if it was a real
		// comparison; otherwise the panel would show a stale "behind" next to
		// a fresh error. Simpler and more honest: a failed check says so.
		c.verdict = Verdict{
			Status:  StatusError,
			Summary: "查不到 GitHub（网络不通、被墙或超时）。这不影响网关运行，稍后会自动重试。",
		}
		return c.stateLocked()
	}
	c.lastErr = ""
	c.remote = &remote
	c.verdict = Evaluate(build, remote)
	return c.stateLocked()
}
