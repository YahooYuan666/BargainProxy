package uperr

import (
	"strings"
)

// IsEdgeBlock reports whether a 403 body is a CDN/WAF edge refusal rather than
// the vendor's own answer. Cloudflare's browser-integrity block ("error code:
// 1010") is the one seen in the field; the check also covers the JSON shape
// that carries the same signal in a structured field, and the generic
// "Attention Required!" interstitial title.
//
// Why this is separate from ClassWAF: a 403 from the vendor's own API usually
// does mean "this credential or this request is not welcome", which is worth a
// short account rest. An edge block means the *edge* did not let the request
// through at all — every account behind the same egress hits it, so charging
// accounts one by one is what turns one edge rule into a dead pool (sub2api
// e0fa4b94f; field finding 2026-10-21).
func IsEdgeBlock(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	if strings.Contains(text, "error code: 1010") ||
		strings.Contains(text, "error code 1010") ||
		strings.Contains(text, "cf-error-details") ||
		strings.Contains(text, "attention required! | cloudflare") {
		return true
	}
	// Structured shapes: {"error":{"cloudflare_error":true}} and the
	// {"cloudflare_error":true} some relays pass through.
	return strings.Contains(text, `"cloudflare_error":true`) ||
		strings.Contains(text, `"cloudflare_error": true`)
}

// EdgeBlocked builds the ClassEdgeBlock error for a body that IsEdgeBlock
// recognized.
func EdgeBlocked(status int, format string, args ...any) *Error {
	return New(ClassEdgeBlock, status, format, args...)
}
