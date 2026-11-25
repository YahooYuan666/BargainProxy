package autoclaw

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// jwtClaim reads one claim out of a JWT without verifying the signature.
//
// Verification is not the point here: the token came out of the local client's
// own credential store, which was decrypted under the current Windows user, so
// it is already as trustworthy as the running process. What we need is the
// claim's VALUE — the device identity the server recorded for that sign-in.
//
// Returns "" for anything that is not a decodable three-part JWT.
func jwtClaim(token, claim string) string {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimPrefix(token, "bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some producers pad; try the padded alphabet before giving up.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if s, ok := payload[claim].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// deviceIDFromTokens returns the device identity recorded in the account's own
// tokens, preferring the access token.
//
// This is the authoritative value, not one we can invent: the server writes it
// into the token at sign-in and reads it back on refresh. Importing a client's
// credentials without it leaves the refresh path signing with a derived id that
// does not match the server's record (see DeviceIDForPhone for the CN symptom).
func deviceIDFromTokens(access, refresh string) string {
	if id := jwtClaim(access, "device_id"); id != "" {
		return id
	}
	return jwtClaim(refresh, "device_id")
}
