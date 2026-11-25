package autoclaw

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/model"
)

// makeJWT builds a three-part token whose payload carries the given claims.
// The signature is a placeholder: jwtClaim does not verify it, because the
// token came out of the local client's own store (see the note on jwtClaim).
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(header) + "." + enc.EncodeToString(payload) + ".c2lnbmF0dXJl"
}

// TestJWTClaimReadsDeviceID is the fact that unblocked the multi-account
// question: the account's device identity is IN its token, so importing does
// not have to invent one.
func TestJWTClaimReadsDeviceID(t *testing.T) {
	const want = "3b7cfdf9-1df6-5cf2-9e5a-b2e28c629928"
	tok := makeJWT(t, map[string]any{
		"device_id": want,
		"user_id":   182537,
		"exp":       1795637417,
	})
	if got := jwtClaim(tok, "device_id"); got != want {
		t.Fatalf("jwtClaim(device_id) = %q, want %q", got, want)
	}
	// user_id is a number here; the helper only returns strings, and must not
	// panic or invent a value for a non-string claim.
	if got := jwtClaim(tok, "user_id"); got != "" {
		t.Fatalf("jwtClaim on a numeric claim = %q, want empty", got)
	}
}

// TestJWTClaimToleratesBearerPrefixAndJunk: the value is read from a token that
// may still carry the "Bearer " prefix, and from things that are not JWTs at
// all (a CN credential is an opaque string).
func TestJWTClaimToleratesBearerPrefixAndJunk(t *testing.T) {
	tok := makeJWT(t, map[string]any{"device_id": "abc"})
	if got := jwtClaim("Bearer "+tok, "device_id"); got != "abc" {
		t.Fatalf("Bearer-prefixed token = %q, want abc", got)
	}
	for _, junk := range []string{"", "opaque-token", "a.b", "a.b.c.d", "!!!.!!!.!!!"} {
		if got := jwtClaim(junk, "device_id"); got != "" {
			t.Errorf("jwtClaim(%q) = %q, want empty for a non-JWT", junk, got)
		}
	}
}

// TestDeviceIDFromTokensPrefersAccessToken: the access token is what the
// refresh path signs with, so its claim is the one that must win.
func TestDeviceIDFromTokensPrefersAccessToken(t *testing.T) {
	access := makeJWT(t, map[string]any{"device_id": "from-access"})
	refresh := makeJWT(t, map[string]any{"device_id": "from-refresh"})
	if got := deviceIDFromTokens(access, refresh); got != "from-access" {
		t.Fatalf("deviceIDFromTokens = %q, want from-access", got)
	}
	// Falls back to the refresh token when the access token cannot be read.
	if got := deviceIDFromTokens("opaque", refresh); got != "from-refresh" {
		t.Fatalf("fallback = %q, want from-refresh", got)
	}
	if got := deviceIDFromTokens("opaque", "opaque"); got != "" {
		t.Fatalf("both unreadable = %q, want empty", got)
	}
}

// TestImportTakesDeviceIDFromTheToken: the importer must record the identity
// the SERVER holds, read from the token, not a value it makes up. A made-up id
// breaks refresh; a shared one makes two accounts one device.
func TestImportTakesDeviceIDFromTheToken(t *testing.T) {
	const want = "aaaa-bbbb-cccc"
	cred := &importedCredential{
		AccessToken:  makeJWT(t, map[string]any{"device_id": want}),
		RefreshToken: makeJWT(t, map[string]any{"device_id": want}),
		UserID:       "880098",
		// Note: no DeviceID field set — the client often has none.
	}
	ia, ok := accountFromCredential(cred, profileFields{}, "/tmp/x", realmOverseas)
	if !ok {
		t.Fatal("a complete credential must import")
	}
	if ia.DeviceID != want {
		t.Fatalf("ImportedAccount.DeviceID = %q, want %q", ia.DeviceID, want)
	}
	cr, err := ParseCreds(ia.Account.CredsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cr.DeviceID != want {
		t.Fatalf("Creds.DeviceID = %q, want %q (refresh signs with this)", cr.DeviceID, want)
	}
}

// TestImportPrefersTheClientsOwnDeviceField: when the client stores the id
// explicitly, that value wins over the claim (it is the same value, but taking
// the client's own field first keeps a future client shape working).
func TestImportPrefersTheClientsOwnDeviceField(t *testing.T) {
	cred := &importedCredential{
		AccessToken:  makeJWT(t, map[string]any{"device_id": "from-token"}),
		RefreshToken: makeJWT(t, map[string]any{"device_id": "from-token"}),
		DeviceID:     "from-client",
		UserID:       "1",
	}
	ia, ok := accountFromCredential(cred, profileFields{}, "/tmp/x", realmOverseas)
	if !ok {
		t.Fatal("import failed")
	}
	if ia.DeviceID != "from-client" {
		t.Fatalf("DeviceID = %q, want the client's own field", ia.DeviceID)
	}
}

// TestSharedDeviceIDsFlagsCollisionsOnly: one account per device is normal and
// silent; two accounts on one device is the case the operator must hear about.
func TestSharedDeviceIDsFlagsCollisionsOnly(t *testing.T) {
	mk := func(id, device string) ImportedAccount {
		return ImportedAccount{
			DeviceID: device,
			Account:  &model.Account{ID: model.ChannelAutoClaw + ":" + id},
		}
	}
	accounts := []ImportedAccount{
		mk("one", "shared-device"),
		mk("two", "shared-device"),
		mk("three", "own-device"),
		mk("four", ""), // no identity recorded: cannot collide
	}
	got := SharedDeviceIDs(accounts)
	if len(got) != 1 {
		t.Fatalf("got %d shared devices, want 1: %v", len(got), got)
	}
	owners, ok := got["shared-device"]
	if !ok {
		t.Fatalf("the shared device was not reported: %v", got)
	}
	if len(owners) != 2 {
		t.Fatalf("owners = %v, want two", owners)
	}
	joined := strings.Join(owners, ",")
	if !strings.Contains(joined, "one") || !strings.Contains(joined, "two") {
		t.Fatalf("owners = %v, want the two that share it", owners)
	}
}

// TestSharedDeviceIDsEmptyWhenAllDistinct: the happy path — every sign-in minted
// its own identity — must produce no warnings at all.
func TestSharedDeviceIDsEmptyWhenAllDistinct(t *testing.T) {
	accounts := []ImportedAccount{
		{DeviceID: "a", Account: &model.Account{ID: "autoclaw:1"}},
		{DeviceID: "b", Account: &model.Account{ID: "autoclaw:2"}},
	}
	if got := SharedDeviceIDs(accounts); len(got) != 0 {
		t.Fatalf("distinct devices reported as shared: %v", got)
	}
}
