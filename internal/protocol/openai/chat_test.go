package openai

import (
	"strings"
	"testing"
)

// TestSessionKeyThreeLevels pins the ingress fingerprint fallback chain:
// explicit ids → cache_control marked part → system+first-user prefix.
func TestSessionKeyThreeLevels(t *testing.T) {
	// Level 1: explicit id wins over everything.
	req, err := ParseChatRequest([]byte(`{"model":"m","conversation_id":"c1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.SessionKey != "conv:c1" {
		t.Fatalf("level 1 = %q", req.SessionKey)
	}

	// Level 2: cache_control marked part (before any explicit id).
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"stable anchor","cache_control":{"type":"ephemeral"}},{"type":"text","text":"moving tail"}]}]}`
	r2, err := ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasPrefix(r2.SessionKey, "cc:") {
		t.Fatalf("level 2 = %q", r2.SessionKey)
	}
	// Same anchor with a different tail → same fingerprint.
	body2 := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"stable anchor","cache_control":{"type":"ephemeral"}},{"type":"text","text":"different tail"}]}]}`
	r2b, err := ParseChatRequest([]byte(body2))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r2b.SessionKey != r2.SessionKey {
		t.Fatalf("marked-anchor fingerprint must not move with the tail: %q vs %q", r2b.SessionKey, r2.SessionKey)
	}

	// Level 3: no explicit id, no marker → prefix digest; stable across tail.
	plain1 := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`
	plain2 := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`
	r3, err := ParseChatRequest([]byte(plain1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r3b, err := ParseChatRequest([]byte(plain2))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasPrefix(r3.SessionKey, "pfx:") {
		t.Fatalf("level 3 = %q", r3.SessionKey)
	}
	if r3b.SessionKey != r3.SessionKey {
		t.Fatalf("prefix fingerprint drifted with the tail: %q vs %q", r3b.SessionKey, r3.SessionKey)
	}

	// Different opening → different fingerprint.
	other, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"different"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if other.SessionKey == r3.SessionKey {
		t.Fatalf("different prefixes must not share a fingerprint: %q", other.SessionKey)
	}

	// Empty conversation → no stickiness.
	empty, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":""}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if empty.SessionKey != "" {
		t.Fatalf("empty conversation must not stick, got %q", empty.SessionKey)
	}
}
