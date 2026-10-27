package sessionfp

// BP-143: a Claude Code subagent's turn must not take over the lead
// conversation's account binding.
//
// Claude Code sends x-claude-code-session-id on every request of the session,
// its subagents' included; only x-claude-code-agent-id marks a request as a
// subagent's. Stickiness is per (channel, session), so without the split a
// subagent's answer overwrote the lead's binding and the lead's next turn was
// judged by the subagent's (small, cold) cache — it moved to another account
// and re-wrote the whole history at the cache-write price. Upstream measured
// two Pro accounts' 5 hours spent in ten minutes (yetone/magpie 3b717d5).

import (
	"net/http"
	"strings"
	"testing"
)

func headerGetter(h http.Header) func(string) string { return h.Get }

// TestLeadKeyIsUnchanged pins the compatibility rule: a lead conversation
// (no agent id) resolves to exactly the key it always did, so bindings
// written before this change keep working.
func TestLeadKeyIsUnchanged(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "sess-abc")
	got := FromHeader(headerGetter(h))
	want := digest("sid:", "sess-abc")
	if got != want {
		t.Fatalf("lead key changed: got %q want %q", got, want)
	}
	if strings.Contains(got, "#sub:") {
		t.Fatalf("lead key must not carry a subagent segment: %q", got)
	}
}

// TestSubagentGetsItsOwnKey is the bug: a subagent request must land on a
// different key than the lead's.
func TestSubagentGetsItsOwnKey(t *testing.T) {
	lead := http.Header{}
	lead.Set("X-Claude-Code-Session-Id", "sess-abc")

	sub := http.Header{}
	sub.Set("X-Claude-Code-Session-Id", "sess-abc")
	sub.Set("X-Claude-Code-Agent-Id", "agent-xyz")

	leadKey, subKey := FromHeader(headerGetter(lead)), FromHeader(headerGetter(sub))
	if leadKey == subKey {
		t.Fatalf("subagent shares the lead's key (%q): it would overwrite the lead's binding", leadKey)
	}
	if !strings.HasPrefix(subKey, leadKey+"#sub:") {
		t.Fatalf("subagent key = %q, want prefix %q + #sub:", subKey, leadKey)
	}
}

// TestEachSubagentIsSeparate: two subagents of one lead must not share a
// binding either — each has its own context and cache.
func TestEachSubagentIsSeparate(t *testing.T) {
	mk := func(agent string) string {
		h := http.Header{}
		h.Set("X-Claude-Code-Session-Id", "sess-abc")
		h.Set("X-Claude-Code-Agent-Id", agent)
		return FromHeader(headerGetter(h))
	}
	a, b := mk("agent-1"), mk("agent-2")
	if a == b {
		t.Fatalf("two subagents share one key: %q", a)
	}
	if a != mk("agent-1") {
		t.Fatalf("subagent key is not stable across requests")
	}
}

// TestSubagentWithoutSessionFallsBackToNothing: an agent id alone names no
// conversation, so it must not create one out of thin air.
func TestSubagentWithoutSessionFallsBackToNothing(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Agent-Id", "agent-xyz")
	if got := FromHeader(headerGetter(h)); got != "" {
		t.Fatalf("agent id alone produced a session key: %q", got)
	}
}

// TestSubagentValueIsHashed: a raw client id must never reach logs or
// upstream headers.
func TestSubagentValueIsHashed(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "sess-abc")
	h.Set("X-Claude-Code-Agent-Id", "super-secret-agent-id")
	got := FromHeader(headerGetter(h))
	if strings.Contains(got, "super-secret") {
		t.Fatalf("raw agent id leaked into the key: %q", got)
	}
}
