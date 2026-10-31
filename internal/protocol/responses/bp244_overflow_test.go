package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// BP-244: a context overflow must carry code `context_length_exceeded`.
//
// magpie 2cca1117 (#4425), production case: Codex compacts only on that exact
// code. With `server_error`/`upstream_error` it resent the same oversized body
// five times ("reconnecting 5/5"), each refused, and never compacted — burning
// every binding on a request that could never succeed until the client shrank
// it. Our writer used to always emit `stream_error`.
func TestContextOverflowCodeIsWhatCodexCompactsOn(t *testing.T) {
	body := RenderContextOverflow("prompt is too long: 312390 > 262144 maximum")
	if body.Error.Code != "context_length_exceeded" {
		t.Fatalf("code = %v, want context_length_exceeded", body.Error.Code)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error (the REQUEST did not fit)", body.Error.Type)
	}
	// It must survive the wire as a string, not a number or null.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"code":"context_length_exceeded"`) {
		t.Fatalf("wire form lost the code: %s", raw)
	}
}

// The streaming path must use the same code: a mid-stream overflow is still an
// overflow, and a client reading response.failed branches on the same field.
func TestStreamFailedCarriesOverflowCode(t *testing.T) {
	var sb strings.Builder
	sw := NewStreamWriter(&sb, nil, "resp_1", "glm-5.3")
	if err := sw.FinishWithErrorCode("context_length_exceeded", "prompt too long"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"context_length_exceeded"`) {
		t.Fatalf("response.failed did not carry the overflow code:\n%s", out)
	}
	if !strings.Contains(out, "response.failed") {
		t.Fatalf("no response.failed event:\n%s", out)
	}
}

// An ordinary failure keeps the old code — this fix must not relabel every
// stream error as an overflow.
func TestStreamFailedKeepsStreamErrorForOtherFailures(t *testing.T) {
	var sb strings.Builder
	sw := NewStreamWriter(&sb, nil, "resp_2", "glm-5.3")
	if err := sw.FinishWithError("upstream died"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, "context_length_exceeded") {
		t.Fatalf("a non-overflow failure was labelled as an overflow:\n%s", out)
	}
	if !strings.Contains(out, "stream_error") {
		t.Fatalf("stream_error code missing:\n%s", out)
	}
}
