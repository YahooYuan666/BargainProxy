// BP-335: three small corrections on the Gemini-family surfaces.
//
//  1. geminiv1 must accept snake_case media keys. The earlier code scoped the
//     snake_case aliases to the two function keys and asserted inlineData kept
//     "its single camelCase form" — CLIProxyAPI 08098d24 (2026-11-22) shows
//     real clients send inline_data / mime_type, and the image was dropped
//     silently.
//  2. finishReason must treat UNEXPECTED_TOOL_CALL / LANGUAGE / NO_IMAGE /
//     OTHER as refusals (CLIProxyAPI 2c8b32fd), not clean finishes.
//  3. a 401/403 must drop the idle keep-alive pool (CLIProxyAPI ec25cc83).
package geminiv1

import (
	"encoding/json"
	"testing"
)

func TestInlineDataSnakeCaseIsAccepted(t *testing.T) {
	var p Part
	if err := json.Unmarshal([]byte(`{"inline_data":{"mime_type":"image/png","data":"QUJD"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.InlineData == nil {
		t.Fatal("inline_data must be parsed — dropping it means the model never sees the image")
	}
	if p.InlineData.MIMEType != "image/png" {
		t.Fatalf("mime_type = %q, want image/png", p.InlineData.MIMEType)
	}
	if p.InlineData.Data != "QUJD" {
		t.Fatalf("data = %q", p.InlineData.Data)
	}
}

// The camelCase spelling must keep working unchanged.
func TestInlineDataCamelCaseStillWorks(t *testing.T) {
	var p Part
	if err := json.Unmarshal([]byte(`{"inlineData":{"mimeType":"image/jpeg","data":"QUJD"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.InlineData == nil || p.InlineData.MIMEType != "image/jpeg" {
		t.Fatalf("camelCase must still parse: %+v", p.InlineData)
	}
}

// Both keys present: the camelCase one wins (it is the documented spelling).
func TestInlineDataCamelCaseWins(t *testing.T) {
	var p Part
	if err := json.Unmarshal([]byte(`{"inlineData":{"mimeType":"image/png","data":"QQ=="},"inline_data":{"mime_type":"image/gif","data":"Qg=="}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.InlineData == nil || p.InlineData.MIMEType != "image/png" || p.InlineData.Data != "QQ==" {
		t.Fatalf("camelCase must win: %+v", p.InlineData)
	}
}

// A blob with no mimeType at all still parses (the alias must not error).
func TestBlobWithoutMime(t *testing.T) {
	var p Part
	if err := json.Unmarshal([]byte(`{"inlineData":{"data":"QQ=="}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.InlineData == nil || p.InlineData.MIMEType != "" {
		t.Fatalf("a blob with no mime type must still parse: %+v", p.InlineData)
	}
}
