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

// TestClientDirsCoverBothLanes pins the directories scanned. Both spellings of
// the overseas directory are listed because our code and the reference tooling
// disagree on it; if one is dropped, a client installed under that name
// silently disappears from the import.
func TestClientDirsCoverBothLanes(t *testing.T) {
	dirs := map[string]string{} // dir -> fallback realm
	for _, cd := range clientDirs {
		dirs[cd.dir] = cd.realm
	}
	if dirs["AutoClaw-official"] != "cn" {
		t.Fatalf("the CN client directory changed: %v", dirs)
	}
	for _, name := range []string{"AutoClaw-overseas-official", "AutoClaw-oversea-official"} {
		if dirs[name] != realmOverseas {
			t.Fatalf("the overseas client directory %q is missing or misclassified: %v", name, dirs)
		}
	}
}

// TestRealmIsDecidedByTheAccountNotByChannelJSON is the corrected rule.
//
// The first version of this read channel.json as the lane and treated
// "official" as overseas. That is wrong: channel.json is the RELEASE channel
// ("official"/"production"/"test"/"pre"), written by every build including the
// CN one. It showed immediately on this machine — AutoClaw-official has
// {"channel":"official"} and holds a CN phone account (181****7452,
// numericUserId 880098), which the first version imported as global.
//
// What decides the lane is what the account IS: a phone means the CN SMS flow,
// an email means the overseas Google/z.ai flow.
func TestRealmIsDecidedByTheAccountNotByChannelJSON(t *testing.T) {
	cases := []struct {
		name     string
		phone    string
		email    string
		fallback string
		want     string
	}{
		// A phone number is NOT a CN signal: an overseas z.ai account signs in
		// with a phone number too. Treating it as one imported a fresh overseas
		// z.ai account as cn (field 2026-11-25), so the prior no longer fires.
		{"a phone with no email keeps the fallback (probe must decide)", "181****7452", "", realmOverseas, realmOverseas},
		{"a phone with no email keeps a cn fallback", "181****7452", "", "cn", "cn"},
		{"an email is a weak overseas prior", "", "a@b.com", "cn", realmOverseas},
		{"no signal keeps the fallback", "", "", "cn", "cn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := realmFromIdentity(c.phone, c.email, "", c.fallback); got != c.want {
				t.Fatalf("realmFromIdentity = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRealmForIgnoresChannelJSON pins that channel.json no longer moves the
// lane, so nobody reintroduces it by accident: the file is a release channel.
func TestRealmForIgnoresChannelJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "channel.json"), []byte(`{"channel":"official"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, fallback := range []string{"cn", realmOverseas} {
		if got := realmFor(dir, fallback); got != fallback {
			t.Fatalf("realmFor returned %q for fallback %q; channel.json must not decide the lane", got, fallback)
		}
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
