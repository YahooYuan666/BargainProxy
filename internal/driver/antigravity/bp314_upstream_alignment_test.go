package antigravity

// BP-314: align the payload with AIClient2API (the project this driver was
// ported from). Two fields were missing, and the project id could never be
// discovered at all.
//
// Field 2026-11-12: the user reported gemini-3.8-flash-high failing through the
// gateway while the SAME account (y200901152022@gmail.com) answered fine in the
// Antigravity IDE. Diffing our request dump against the reference implementation
// showed three gaps:
//
//  1. request.sessionId — the reference always sets it
//     (sha256(first user text) → first 8 bytes → "-" + positive uint64). We
//     never sent it, so every turn looked like a brand-new conversation and the
//     upstream could not key its prefix cache on it.
//  2. project — the reference puts the discovered project id at the top level
//     (and DELETES the field when it has none). We had the struct field but it
//     was always empty, so `omitempty` dropped it.
//  3. loadCodeAssist — our call omitted the User-Agent, and the upstream answers
//     that with 200 and an EMPTY cloudaicompanionProject, silently. So discovery
//     "succeeded" with "" and gap 2 could never close. Measured with one
//     variable: no UA → "" on all three accounts and both hosts; with UA →
//     "aicode-consumers" on all three and both hosts.

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

// The stable session id must match the reference algorithm exactly:
// sha256(first user text), first 8 bytes big-endian, masked positive, dash.
func TestStableSessionIDMatchesReferenceAlgorithm(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "system prompt"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "在么"}}},
		},
	}
	raw, err := buildRequest("aicode-consumers", "gemini-3.8-flash-high", req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	sid := sessionIDOf(t, raw)
	// Reference: sha256("在么") first 8 bytes, & 0x7FFFFFFFFFFFFFFF, "-"+decimal.
	// Hard-coded so a silent algorithm change (e.g. hashing the wrong message)
	// shows up here instead of as a cache miss upstream.
	const want = "-8210116156397361059"
	if sid != want {
		t.Fatalf("sessionId = %q, want %q (sha256 of the first USER text, not the system one)", sid, want)
	}
}

// Stability is the whole point: identical conversation prefix → identical id,
// so the upstream can treat consecutive turns as one session.
func TestStableSessionIDIsStable(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "first turn"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "reply"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "second turn"}}},
		},
	}
	first := sessionIDOf(t, mustBuild(t, req))
	second := sessionIDOf(t, mustBuild(t, req))
	if first == "" || first != second {
		t.Fatalf("sessionId not stable across identical builds: %q vs %q", first, second)
	}

	// A different FIRST user message is a different conversation.
	other := &ir.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "another conversation"}}},
		},
	}
	if got := sessionIDOf(t, mustBuild(t, other)); got == first {
		t.Fatalf("different first user text produced the same sessionId %q", got)
	}
}

// The project id must reach the wire. This is the field whose absence made the
// dump differ from the IDE's request.
func TestProjectReachesTheWire(t *testing.T) {
	req := &ir.ChatRequest{
		Model:    "gemini-3.8-flash-high",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	withProject, err := buildRequest("aicode-consumers", "gemini-3.8-flash-high", req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var outer struct {
		Project string `json:"project"`
	}
	json.Unmarshal(withProject, &outer)
	if outer.Project != "aicode-consumers" {
		t.Fatalf("project = %q, want aicode-consumers on the wire", outer.Project)
	}

	// No project known → the field is omitted entirely, matching the reference's
	// `delete template.project` (an explicit empty string is not the same wire
	// shape and the reference never sends one).
	noProject, err := buildRequest("", "gemini-3.8-flash-high", req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var outer2 map[string]json.RawMessage
	json.Unmarshal(noProject, &outer2)
	if _, ok := outer2["project"]; ok {
		t.Fatalf("project field present with no value: %s", noProject)
	}
}

func mustBuild(t *testing.T, req *ir.ChatRequest) []byte {
	t.Helper()
	raw, err := buildRequest("", req.Model, req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	return raw
}

func sessionIDOf(t *testing.T, raw []byte) string {
	t.Helper()
	var outer struct {
		Request struct {
			SessionID string `json:"sessionId"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return outer.Request.SessionID
}
