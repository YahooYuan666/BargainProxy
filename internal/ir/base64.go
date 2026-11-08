// base64.go — inline-image base64 normalization (BP-297).
//
// magpie b657a21 (#5697): Google's own Python SDK (google-genai) writes a
// part's bytes with base64.urlsafe_b64encode — '-' and '_' where standard
// base64 has '+' and '/' — and Google's API reads either, but vendors and
// relays in between may read only the standard alphabet: the image comes
// back 400 "Failed to base64-decode image data", the same request with '+'
// and '/' answered. Unpadded standard base64 trips the same wall. Every
// ingest site that copies client-supplied inline bytes into an ImageB64 part
// runs them through StdBase64 first.
package ir

import (
	"encoding/base64"
	"strings"
)

// StdBase64 returns s as standard, padded base64 (RFC 4648 §4) when it is
// the URL-safe alphabet (§5) or unpadded. Standard-and-padded input goes
// back byte for byte; mixed alphabets or undecodable input go back unchanged
// (the upstream then reports the error it actually saw, not one we invented).
func StdBase64(s string) string {
	if s == "" {
		return s
	}
	urlSafe := strings.ContainsAny(s, "-_")
	if !urlSafe && len(s)%4 == 0 {
		return s // standard and padded already
	}
	if urlSafe && strings.ContainsAny(s, "+/") {
		return s // both alphabets: neither's
	}
	var (
		raw []byte
		err error
	)
	if urlSafe {
		if raw, err = base64.RawURLEncoding.DecodeString(s); err != nil {
			raw, err = base64.URLEncoding.DecodeString(s)
		}
	} else if raw, err = base64.RawStdEncoding.DecodeString(s); err != nil {
		return s
	}
	if err != nil {
		return s
	}
	return base64.StdEncoding.EncodeToString(raw)
}
