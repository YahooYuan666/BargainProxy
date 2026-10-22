package version

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FeedURL is the commits Atom feed of Branch. It is used instead of the REST
// API on purpose: api.github.com allows 60 unauthenticated requests per hour
// per IP (a shared NAT or a proxied exit burns that quickly), while the Atom
// feed carries no such budget. The feed needs no token either, which matters
// because the gateway must never store credentials it does not already have.
func FeedURL() string {
	return fmt.Sprintf("https://github.com/%s/commits/%s.atom", Repo, Branch)
}

// CommitURL is the human-readable page for a commit.
func CommitURL(sha string) string {
	return fmt.Sprintf("https://github.com/%s/commit/%s", Repo, sha)
}

// RepoURL is the repository landing page (for the "watch the repo" hint).
func RepoURL() string { return "https://github.com/" + Repo }

// feedBodyCap bounds the response so a hostile or broken endpoint cannot
// stream unbounded bytes into memory. The real feed is a few tens of KB.
const feedBodyCap = 1 << 20

// atomFeed is the subset of Atom we need. GitHub emits one <entry> per commit,
// newest first.
type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID      string `xml:"id"`
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
	Link    struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
}

// FetchHead reads the newest commit of Branch. The http.Client must be
// caller-supplied so the gateway can reuse its proxy-aware transport.
func FetchHead(ctx context.Context, hc *http.Client) (Commit, error) {
	return FetchHeadFrom(ctx, hc, FeedURL())
}

// FetchHeadFrom is FetchHead against an explicit URL (tests point it at a
// local server; production always passes FeedURL()).
func FetchHeadFrom(ctx context.Context, hc *http.Client, url string) (Commit, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Commit{}, err
	}
	// GitHub rejects requests without a User-Agent; a stable, honest one is
	// also what an operator would expect to see in their own proxy logs.
	req.Header.Set("User-Agent", "BargainProxy-update-check")
	req.Header.Set("Accept", "application/atom+xml")
	resp, err := hc.Do(req)
	if err != nil {
		return Commit{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Commit{}, fmt.Errorf("github feed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, feedBodyCap))
	if err != nil {
		return Commit{}, err
	}
	return ParseFeed(body)
}

// ParseFeed extracts the newest commit from a commits Atom feed.
func ParseFeed(body []byte) (Commit, error) {
	var f atomFeed
	if err := xml.Unmarshal(body, &f); err != nil {
		return Commit{}, fmt.Errorf("github feed: parse: %w", err)
	}
	if len(f.Entries) == 0 {
		return Commit{}, fmt.Errorf("github feed: no entries")
	}
	e := f.Entries[0]
	sha := shaFromCommitID(e.ID)
	if sha == "" {
		return Commit{}, fmt.Errorf("github feed: first entry has no commit id")
	}
	c := Commit{
		SHA:  sha,
		Time: parseAtomTime(e.Updated),
		URL:  strings.TrimSpace(e.Link.Href),
	}
	if c.URL == "" {
		c.URL = CommitURL(sha)
	}
	// Titles are pretty-printed across lines by GitHub; collapse the
	// whitespace so the panel can render it on one line.
	c.Title = strings.Join(strings.Fields(e.Title), " ")
	if c.Title == "" {
		c.Title = "(无提交标题)"
	}
	return c, nil
}

// shaFromCommitID pulls the SHA out of the feed's tag URI, which looks like
//
//	tag:github.com,2008:Grit::Commit/a975d93f959ac0016dc287c6a473bec925892def
//
// and is, unusually for a tag URI, the only place the SHA appears at all.
func shaFromCommitID(id string) string {
	i := strings.LastIndex(id, "/")
	if i < 0 || i == len(id)-1 {
		return ""
	}
	sha := strings.TrimSpace(id[i+1:])
	if len(sha) < 7 {
		return ""
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return ""
		}
	}
	return sha
}

func parseAtomTime(v string) time.Time {
	v = strings.TrimSpace(v)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
