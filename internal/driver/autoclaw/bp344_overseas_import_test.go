package autoclaw

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestImportOverseasReadsOnlyTheOverseasStore is the regression for the bug the
// operator hit: the panel's import put a DOMESTIC account where an overseas one
// was expected.
//
// The two stores are separate files in separate directories:
//
//	overseas   %APPDATA%\AutoClaw\auth.json                  (1.x shape)
//	domestic   %APPDATA%\AutoClaw-official\accounts\<hash>\  (2.x shape)
//
// A single "import" covering both cannot know which one was meant, so the
// overseas import must read the overseas path and nothing else.
func TestImportOverseasReadsOnlyTheOverseasStore(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)

	// The overseas store exists but holds no readable key (no Local State), so
	// the import must fail with a message about the OVERSEAS path rather than
	// falling back to the domestic directory.
	root := filepath.Join(appData, overseasClientDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{
		"deviceId": "dev-overseas",
		"token":    "enc:" + base64.StdEncoding.EncodeToString([]byte("not-a-v10-blob")),
		"userInfo": map[string]any{"user_id": "uuid-1", "user_name": "Overseas Person"},
	}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(root, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	accounts, notes := ImportOverseas()
	if len(accounts) != 0 {
		t.Fatalf("imported %d accounts from an undecryptable store", len(accounts))
	}
	joined := ""
	for _, n := range notes {
		joined += n + " "
	}
	if joined == "" {
		t.Fatal("a failed overseas import must say why")
	}
	// The report must name the overseas path, not the domestic one — that is how
	// the operator tells which store was even looked at.
	if !containsAny(joined, "%APPDATA%", "AutoClaw", "解密", "密钥") {
		t.Fatalf("the note does not describe the overseas store: %q", joined)
	}
}

// TestImportOverseasSaysWhenNotSignedIn: with no auth.json the note must name
// the missing path so the operator knows which client to sign into.
func TestImportOverseasSaysWhenNotSignedIn(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	accounts, notes := ImportOverseas()
	if len(accounts) != 0 {
		t.Fatalf("imported from a machine with no overseas sign-in")
	}
	if len(notes) == 0 {
		t.Fatal("no explanation for an empty overseas import")
	}
	if !containsAny(notes[0], "auth.json", "海外版") {
		t.Fatalf("the note does not point at the overseas sign-in: %q", notes[0])
	}
}

// TestOverseasStorePathIsTheOneTheClientWrites pins the directory name. If it is
// wrong the import silently reads nothing (or, worse, the domestic store).
func TestOverseasStorePathIsTheOneTheClientWrites(t *testing.T) {
	if overseasClientDir != "AutoClaw" {
		t.Fatalf("overseasClientDir = %q; the global client writes to %%APPDATA%%\\AutoClaw", overseasClientDir)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
