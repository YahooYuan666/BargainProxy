// BP-357 (CLIProxyAPI ff9339fe): the Anthropic surface has no dedicated code
// for "the prompt did not fit", so the WORDING is the contract — Claude Code
// matches "prompt is too long" and compacts. A generic 400 makes it resend the
// identical oversized body.
package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBP357OverflowCarriesTheMatchingPhrase(t *testing.T) {
	body := RenderContextOverflow("antigravity: Request contains an invalid argument.")
	if !strings.Contains(body.Error.Message, "prompt is too long") {
		t.Fatalf("message = %q — Claude Code matches this phrase and compacts without it", body.Error.Message)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error (the request is at fault)", body.Error.Type)
	}
	// The upstream's own words must survive the prefix — that is the evidence
	// a human reads in the log.
	if !strings.Contains(body.Error.Message, "invalid argument") {
		t.Fatalf("upstream wording lost: %q", body.Error.Message)
	}
}

// A message that already says it must not be prefixed twice (the same error
// can pass through two rendering paths on a retry).
func TestBP357NoDoublePrefix(t *testing.T) {
	body := RenderContextOverflow("prompt is too long: 300000 tokens > 200000")
	if strings.Count(strings.ToLower(body.Error.Message), "prompt is too long") != 1 {
		t.Fatalf("message = %q", body.Error.Message)
	}
}

// The envelope shape is the Anthropic error envelope, not a bare string.
func TestBP357EnvelopeShape(t *testing.T) {
	b, err := json.Marshal(RenderContextOverflow("x"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Type != "error" || doc.Error.Type != "invalid_request_error" {
		t.Fatalf("envelope = %s", b)
	}
}
