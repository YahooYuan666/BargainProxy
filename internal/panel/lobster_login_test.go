package panel

import (
	"net/url"
	"strings"
	"testing"
)

// TestLobsterAuthorizeURLUsesHashQuery pins the one detail that silently
// breaks the whole login: the portal router reads the login parameters from
// the HASH query, not the URL query. Putting them in the URL query returns a
// page that looks fine and never calls back.
func TestLobsterAuthorizeURLUsesHashQuery(t *testing.T) {
	got := lobsterAuthorizeURL(
		"https://lobsterai.youdao.com",
		"http://127.0.0.1:54321/auth/callback",
		"state-abc",
	)
	head, hash, ok := strings.Cut(got, "#")
	if !ok {
		t.Fatalf("no hash in %q", got)
	}
	if strings.Contains(head, "?") {
		t.Fatalf("parameters leaked into the URL query: %q", head)
	}
	if head != "https://lobsterai.youdao.com/portal" {
		t.Fatalf("head = %q", head)
	}
	route, query, ok := strings.Cut(hash, "?")
	if !ok || route != "/login" {
		t.Fatalf("hash route = %q (full %q)", route, hash)
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("hash query unparseable: %v", err)
	}
	// source=electron is mandatory; without it the portal drops redirect_uri.
	if q.Get("source") != "electron" {
		t.Fatalf("source = %q, want electron", q.Get("source"))
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:54321/auth/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != "state-abc" {
		t.Fatalf("state = %q", q.Get("state"))
	}
}

// TestLobsterAuthorizeURLKeepsExistingHashParams: the portal base may carry a
// path prefix; the login route is always appended after it, and our params
// must land in the hash query.
func TestLobsterAuthorizeURLKeepsExistingHashParams(t *testing.T) {
	got := lobsterAuthorizeURL(
		"https://example.test/base",
		"http://127.0.0.1:1/auth/callback",
		"s1",
	)
	if !strings.Contains(got, "/base/portal#/login?") {
		t.Fatalf("path composition wrong: %q", got)
	}
	q, err := url.ParseQuery(strings.SplitN(got, "?", 2)[1])
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if q.Get("source") != "electron" || q.Get("state") != "s1" {
		t.Fatalf("query = %v", q)
	}
}
