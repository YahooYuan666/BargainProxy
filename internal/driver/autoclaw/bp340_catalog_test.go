package autoclaw

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCNModelsMatchTheLiveCatalog pins the static fallback to what the gateway
// actually serves (read 2026-11-25 from
// GET /autoclaw-proxy/proxy/autoclaw-model-config, X-Channel: AutoClaw26).
//
// This is the table that was wrong for months: it carried ids the upstream no
// longer lists (openclaw, openclaw/default, glm-5.2) and it was missing the
// two models that were added (GLM-5.4 and GLM-5.3-FlashX-Legal). The reason
// GLM-5.4 could hide is worth keeping in the test: it ships as
// zai_glm-x-preview, so a table built from version numbers would never find it.
func TestCNModelsMatchTheLiveCatalog(t *testing.T) {
	want := []string{
		"zai_glm-x-preview",        // GLM-5.4 — the id carries no version number
		"zaicoding_glm-5.3",        // GLM-5.3
		"zai_glm-5.3-flash",        // GLM-5.3-Flash
		"zai_glm-5.3-flashx-legal", // GLM-5.3-FlashX-Legal
		"zai_auto",                 // Auto
		"zai_auto-fast",            // Auto-Fast
	}
	have := map[string]bool{}
	for _, m := range CNModels {
		have[m] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("CNModels is missing %q, which the live catalog serves", w)
		}
	}
}

// TestCNModelsDropsRetiredIDs: the catalog does not list these and a request
// for them is refused, so keeping them only produced a doomed upstream attempt
// per request that referenced them.
func TestCNModelsDropsRetiredIDs(t *testing.T) {
	retired := []string{
		"openclaw",         // never a chat route
		"openclaw/default", // ditto
		"glm-5.2",          // gone from the catalog
	}
	for _, r := range retired {
		for _, m := range CNModels {
			if m == r {
				t.Errorf("CNModels still lists %q, which the live catalog does not serve", r)
			}
		}
	}
}

// TestCatalogPathIsTheManagedGatewayRoute pins the discovery endpoint. The
// client reads its model picker from this exact path; a typo silently turns
// discovery into a 404 that always falls back to the static table.
func TestCatalogPathIsTheManagedGatewayRoute(t *testing.T) {
	const want = "/autoclaw-proxy/proxy/autoclaw-model-config"
	if catalogPath != want {
		t.Fatalf("catalogPath = %q, want %q", catalogPath, want)
	}
}

// TestCatalogHeadersUsePlainAuthorization is the credential-header split,
// measured live on both lanes 2026-11-25:
//
//	X-Authorization: Bearer <tok> -> 401 Invalid token
//	Authorization:   Bearer <tok> -> 200, the catalogue
//
// The chat routes are the opposite way round, so mixing the two silently
// breaks discovery while chat keeps working.
func TestCatalogHeadersUsePlainAuthorization(t *testing.T) {
	h := catalogHeadersFor("Bearer tok123", "secret", false)
	if got := h.Get("Authorization"); got != "Bearer tok123" {
		t.Fatalf("Authorization = %q, want the token", got)
	}
	if got := h.Get("X-Authorization"); got != "" {
		t.Fatalf("X-Authorization must not be set on the catalog route (it is read first and wins): %q", got)
	}
	if got := h.Get("X-Channel"); got != managedChannel {
		t.Fatalf("X-Channel = %q, want %q", got, managedChannel)
	}
	if h.Get("X-Auth-Sign") == "" {
		t.Fatal("the catalog route still needs the app-secret signature")
	}
}

// TestCatalogHeadersOverseasShape: the two lanes take different fingerprints,
// so the overseas catalog must not carry the CN channel/version pair.
func TestCatalogHeadersOverseasShape(t *testing.T) {
	h := catalogHeadersFor("Bearer tok", "secret", true)
	if got := h.Get("X-Channel"); got != overseasChannel {
		t.Fatalf("overseas X-Channel = %q, want %q", got, overseasChannel)
	}
	if got := h.Get("X-Version"); got != overseasVersion {
		t.Fatalf("overseas X-Version = %q, want %q", got, overseasVersion)
	}
	if got := h.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("overseas Authorization = %q, want the token", got)
	}
}

// TestAccountFromCredentialCarriesTheDecidedLane is the wiring half of the
// realm fix: whatever lane was decided must be the lane the account is stored
// and routed with, in BOTH places it is recorded (the wrapper and the creds
// blob). A test of realmFor alone stays green if the caller drops its answer,
// which is exactly how the field bug survived.
func TestAccountFromCredentialCarriesTheDecidedLane(t *testing.T) {
	cred := &importedCredential{
		AccessToken:  "tok",
		RefreshToken: "ref",
		UserID:       "880098",
		Nickname:     "Ann",
	}
	prof := profileFields{NumericUserID: "880098"}

	ia, ok := accountFromCredential(cred, prof, "/tmp/client", realmOverseas)
	if !ok {
		t.Fatal("a complete credential must import")
	}
	if ia.Realm != realmOverseas {
		t.Fatalf("wrapper realm = %q, want %q", ia.Realm, realmOverseas)
	}
	if ia.Account.Realm != realmOverseas {
		t.Fatalf("account realm = %q, want %q (the pool routes on this one)", ia.Account.Realm, realmOverseas)
	}
	cr, err := ParseCreds(ia.Account.CredsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Region != realmOverseas {
		t.Fatalf("creds region = %q, want %q (chatManaged picks the host from this)", cr.Region, realmOverseas)
	}
	if cr.AccessToken != "Bearer tok" {
		t.Fatalf("token = %q, want a Bearer-prefixed token", cr.AccessToken)
	}
}

// TestAccountFromCredentialRejectsIncomplete: an account with no stable id or
// no token is not importable — a row that cannot be routed or refreshed is
// worse than no row.
func TestAccountFromCredentialRejectsIncomplete(t *testing.T) {
	if _, ok := accountFromCredential(&importedCredential{AccessToken: "tok"}, profileFields{}, "s", "cn"); ok {
		t.Error("an account with no id must not import")
	}
	if _, ok := accountFromCredential(&importedCredential{UserID: "1"}, profileFields{}, "s", "cn"); ok {
		t.Error("an account with no token must not import")
	}
}

// TestScanPassesTheDirectoryRealmAsFallback is the end-to-end wiring test for
// the CORRECTED rule: the scan walks a store under the directory-name realm,
// and accountFromCredential then overrides it from the account's own identity.
//
// The earlier version of this test asserted that channel.json decided the lane.
// That was wrong — channel.json is a release channel, not a region, and it made
// a CN phone account import as global (field 2026-11-25) — so the assertion is
// inverted here: the scan must pass the directory realm through untouched, and
// the identity decides later.
func TestScanPassesTheDirectoryRealmAsFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "channel.json"), []byte(`{"channel":"official"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Local State"),
		[]byte(`{"os_crypt":{"encrypted_key":"RFBBUEkBAABub3RhcmVhbGtleQ=="}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var seen string
	prev := observeImportRealm
	observeImportRealm = func(realm string) { seen = realm }
	t.Cleanup(func() { observeImportRealm = prev })

	dirs := []struct {
		dir   string
		realm string
	}{{filepath.Base(root), "cn"}}
	t.Setenv("APPDATA", filepath.Dir(root))
	importFromClientDirs(dirs)

	if seen != "cn" {
		t.Fatalf("the scan walked the store under %q, want the directory realm %q "+
			"(channel.json must not change it)", seen, "cn")
	}
}