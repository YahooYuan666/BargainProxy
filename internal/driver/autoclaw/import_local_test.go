package autoclaw

import (
	"os"
	"path/filepath"
	"testing"
)

// TestImportFromClientDirDecryptsTheRealShape builds a synthetic client store
// in the exact Chromium/DPAPI shape and checks the importer walks it.
//
// The DPAPI call itself is not exercised here (it needs the real Windows user
// and a real DPAPI blob); that half is covered by the live run against the
// machine's own client. What this pins is the directory walk, the profile
// fallback and the lane assignment, which is where a silent regression would
// hide.
func TestImportFromClientDirWalksAccounts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Local State"),
		[]byte(`{"os_crypt":{"encrypted_key":"RFBBUEkBAABub3RhcmVhbGtleQ=="}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	accDir := filepath.Join(root, "accounts", "deadbeef")
	if err := os.MkdirAll(accDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A credential blob whose magic is wrong must be skipped, not crash.
	if err := os.WriteFile(filepath.Join(accDir, "account-credentials.enc"),
		[]byte("XXXXnot-a-v10-blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accDir, "account-profile.json"),
		[]byte(`{"profile":{"accountId":"acc-1","displayName":"Ann","email":"ann@example.com","numericUserId":182537}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The DPAPI unwrap will fail on this fake key, so the walk must return an
	// error rather than pretending the directory is empty.
	if _, err := importFromClientDir(root, realmOverseas); err == nil {
		t.Fatal("a store with an unusable key must report an error, not silently import nothing")
	}
}

// TestClientDirsCoverBothLanes pins the two directories that decide the lane.
// If one is dropped, that lane's accounts silently disappear from the import.
func TestClientDirsCoverBothLanes(t *testing.T) {
	realms := map[string]string{}
	for _, cd := range clientDirs {
		realms[cd.realm] = cd.dir
	}
	if realms["cn"] != "AutoClaw-official" {
		t.Fatalf("the CN client directory changed: %q", realms["cn"])
	}
	if realms[realmOverseas] != "AutoClaw-oversea-official" {
		t.Fatalf("the oversea client directory changed: %q", realms[realmOverseas])
	}
	if len(clientDirs) != 2 {
		t.Fatalf("expected exactly two client directories, got %d", len(clientDirs))
	}
}

// TestReadProfileHandlesMissingFields: the profile is a convenience, never a
// requirement — a credential that decrypts is enough to import.
func TestReadProfileHandlesMissingFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "account-profile.json")
	if err := os.WriteFile(p, []byte(`{"profile":{"displayName":"Ann"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readProfile(p)
	if got.DisplayName != "Ann" {
		t.Fatalf("displayName = %q", got.DisplayName)
	}
	if got.Email != "" || got.AccountID != "" {
		t.Fatalf("absent fields must stay empty: %+v", got)
	}
	// A missing file is not an error either.
	if got := readProfile(filepath.Join(dir, "nope.json")); got != (profileFields{}) {
		t.Fatalf("missing profile must yield zero value: %+v", got)
	}
}
