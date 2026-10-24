// Credential import for the AutoClaw lanes.
//
// The official desktop client stores its login in the Chromium shape:
//
//	%APPDATA%\<client>\Local State        os_crypt.encrypted_key (DPAPI-wrapped)
//	%APPDATA%\<client>\accounts\<hash>\account-credentials.enc   "v10" + nonce + AES-256-GCM
//
// The key is protected by Windows DPAPI under the *current user*, so only that
// user's process can read it. This file holds the portable half (directory
// discovery, JSON parsing, AES-GCM); the DPAPI call itself lives in
// import_windows.go, and import_other.go makes the whole feature a clean
// "not supported here" elsewhere.
//
// Why import at all instead of a login form: the overseas lane's in-app OAuth
// is closed at the edge (a real-human attempt on 2026-09-20 still came back
// HTTP 405 + an Alibaba edge block), so signing in with the official client
// and reading what it leaves behind is the only path. The CN lane also works
// this way, which means one button serves both.
package autoclaw

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bargainproxy/internal/model"
)

// clientDirs are the official client's per-user data directories, CN first.
// The two clients are separate installs with separate credential stores
// (installing one over the other replaces the executable), so the directory
// name is what tells the lanes apart.
var clientDirs = []struct {
	dir   string
	realm string
}{
	{"AutoClaw-official", "cn"},
	{"AutoClaw-oversea-official", realmOverseas},
}

// importedCredential is the subset of the client's credential blob this driver
// needs. Field names are the client's own camelCase.
type importedCredential struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	DeviceID     string `json:"deviceId"`
	UserID       string `json:"userId"`
	Nickname     string `json:"nickname"`
	Email        string `json:"email"`
}

// ImportedAccount is one account found on this machine.
type ImportedAccount struct {
	Account *model.Account
	Realm   string
	Source  string // which client directory it came from
	Email   string
	// AltIDs are the account's OTHER stable identifiers. The official client
	// records the same person under two id systems — a UUID-shaped accountId
	// and a numeric userId — and the SMS login path stores the UUID while the
	// JWT carries the number. Importing without knowing both would register
	// one person twice (field: 2026-10-24, autoclaw:153f2fb1… and
	// autoclaw:880098 were the same account).
	AltIDs []string
}

// ImportFromLocalClients scans the official clients' credential stores and
// returns every account it can decrypt.
//
// A directory that does not exist is skipped silently (that client is simply
// not installed); a directory that exists but yields nothing is reported, since
// "installed but not signed in" is a state the operator needs to understand.
func ImportFromLocalClients() (accounts []ImportedAccount, notes []string) {
	for _, cd := range clientDirs {
		root, err := clientDataDir(cd.dir)
		if err != nil {
			notes = append(notes, cd.dir+": "+err.Error())
			continue
		}
		if _, err := os.Stat(root); err != nil {
			notes = append(notes, cd.dir+": 未安装（目录不存在）")
			continue
		}
		found, err := importFromClientDir(root, cd.realm)
		if err != nil {
			notes = append(notes, cd.dir+": "+err.Error())
			continue
		}
		if len(found) == 0 {
			notes = append(notes, cd.dir+": 已安装但未登录")
			continue
		}
		accounts = append(accounts, found...)
	}
	return accounts, notes
}

// clientDataDir resolves %APPDATA%\<name>.
func clientDataDir(name string) (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", errors.New("APPDATA is not set (this import only works on Windows)")
	}
	return filepath.Join(appData, name), nil
}

// importFromClientDir reads every account under one client's accounts/ folder.
func importFromClientDir(root, realm string) ([]ImportedAccount, error) {
	key, err := osCryptKey(root)
	if err != nil {
		return nil, err
	}
	accRoot := filepath.Join(root, "accounts")
	entries, err := os.ReadDir(accRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // installed but nobody signed in
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var out []ImportedAccount
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(accRoot, e.Name())
		credPath := filepath.Join(dir, "account-credentials.enc")
		blob, err := os.ReadFile(credPath)
		if err != nil {
			continue // an account folder without credentials is not an account yet
		}
		cred, err := decryptCredentialsBlob(key, blob)
		if err != nil {
			// One unreadable account folder must not hide the rest.
			continue
		}
		prof := readProfile(filepath.Join(dir, "account-profile.json"))

		uid := firstNonEmpty(cred.UserID, prof.NumericUserID, prof.AccountID)
		if uid == "" {
			continue
		}
		token := strings.TrimSpace(cred.AccessToken)
		if token == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
			token = "Bearer " + token
		}
		cr := &Creds{
			AccessToken:  token,
			RefreshToken: cred.RefreshToken,
			DeviceID:     cred.DeviceID,
			UID:          uid,
			Region:       realm,
		}
		var alt []string
		for _, id := range []string{cred.UserID, prof.NumericUserID, prof.AccountID} {
			if id != "" && id != uid {
				alt = append(alt, id)
			}
		}
		out = append(out, ImportedAccount{
			Realm:  realm,
			Source: dir,
			AltIDs: alt,
			Email:  firstNonEmpty(cred.Email, prof.Email),
			Account: &model.Account{
				ID:        model.ChannelAutoClaw + ":" + uid,
				Channel:   model.ChannelAutoClaw,
				UID:       uid,
				Nickname:  firstNonEmpty(cred.Nickname, prof.DisplayName, cred.Email, prof.Email, uid),
				Realm:     realm,
				CredsJSON: cr.Marshal(),
			},
		})
	}
	return out, nil
}

// profileFields is the plaintext half of the client's account folder.
type profileFields struct {
	NumericUserID string
	AccountID     string
	DisplayName   string
	Email         string
}

func readProfile(path string) profileFields {
	raw, err := os.ReadFile(path)
	if err != nil {
		return profileFields{}
	}
	var doc struct {
		Profile struct {
			AccountID     string `json:"accountId"`
			DisplayName   string `json:"displayName"`
			Email         string `json:"email"`
			NumericUserID any    `json:"numericUserId"`
		} `json:"profile"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return profileFields{}
	}
	p := doc.Profile
	return profileFields{
		NumericUserID: fmt.Sprintf("%v", p.NumericUserID),
		AccountID:     p.AccountID,
		DisplayName:   p.DisplayName,
		Email:         p.Email,
	}
}

// osCryptKey reads Local State's DPAPI-wrapped key and unwraps it.
func osCryptKey(root string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("Local State: %w", err)
	}
	var doc struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("Local State parse: %w", err)
	}
	if doc.OSCrypt.EncryptedKey == "" {
		return nil, errors.New("Local State has no os_crypt.encrypted_key")
	}
	wrapped, err := base64.StdEncoding.DecodeString(doc.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("encrypted_key base64: %w", err)
	}
	// The stored value is "DPAPI" + the DPAPI blob.
	const magic = "DPAPI"
	if len(wrapped) < len(magic) || string(wrapped[:len(magic)]) != magic {
		return nil, errors.New("encrypted_key is not DPAPI-wrapped")
	}
	return dpapiUnprotect(wrapped[len(magic):])
}

// decryptCredentialsBlob opens a Chromium "v10" blob: 3-byte magic, 12-byte
// nonce, AES-256-GCM ciphertext with the tag appended.
func decryptCredentialsBlob(key, blob []byte) (*importedCredential, error) {
	const magic = "v10"
	if len(blob) < len(magic)+12+16 {
		return nil, errors.New("credential blob too short")
	}
	if string(blob[:len(magic)]) != magic {
		return nil, fmt.Errorf("unexpected credential magic %q", string(blob[:len(magic)]))
	}
	nonce := blob[len(magic) : len(magic)+12]
	ciphertext := blob[len(magic)+12:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var cred importedCredential
	if err := json.Unmarshal(plain, &cred); err != nil {
		return nil, fmt.Errorf("credential json: %w", err)
	}
	return &cred, nil
}

// CredentialAge is how old the client's credential file is, for the panel to
// show whether the import is reading something fresh.
func CredentialAge(source string) (time.Duration, bool) {
	st, err := os.Stat(filepath.Join(source, "account-credentials.enc"))
	if err != nil {
		return 0, false
	}
	return time.Since(st.ModTime()), true
}
