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
	"context"
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

// clientDirs are the per-user data directories to scan for credential stores.
//
// These are DIRECTORIES TO SCAN, nothing more. The name says nothing about the
// lane, and on this machine there is exactly one that matters:
//
//	%APPDATA%\AutoClaw-official   ← the global client (AutoClawGlobal) writes
//	                                here, and so does the CN one; verified
//	                                2026-11-25: it holds CN phone accounts
//	                                (181****7452, 150****8102) AND the overseas
//	                                account, all under accounts/<hash>/
//
// The two installs are told apart by their DOMAINS, not their data directories:
//
//	D:\ProgramFiles\AutoClawGlobal  → autoglm-api.autoglm.ai     (overseas)
//	D:\ProgramFiles\AutoClaw2       → autoglm-api.zhipuai.cn     (CN)
//
// so the lane of an account cannot come from a path at all — it comes from
// asking the upstream (ProbeRealm). The extra names below are kept because a
// future client build may pick a different directory, and scanning a
// non-existent one costs nothing.
var clientDirs = []struct {
	dir   string
	realm string // fallback only, and only a prior; ProbeRealm decides
}{
	{"AutoClaw-official", "cn"},
	{"AutoClaw", "cn"},
	{"AutoClaw-overseas-official", realmOverseas},
	{"AutoClaw-oversea-official", realmOverseas},
	{"AutoClaw-global", realmOverseas},
	{"AutoClawGlobal", realmOverseas},
}

// realmFor decides an account's lane.
//
// channel.json must NOT be read as a lane: it is the RELEASE CHANNEL
// ("official" / "production" / "test" / "pre"), and "official" is the default
// written by every build including the CN one. Treating it as "overseas" is
// wrong, and it showed immediately: this machine's AutoClaw-official directory
// has {"channel":"official"} and holds a CN phone account (181****7452,
// numericUserId 880098), which the first version of this function classified as
// global. Verified against the client's own parser (main.cjs: the value is
// validated as a cross-platform directory name and has nothing to do with
// regions).
//
// What actually identifies the lane is the ACCOUNT ITSELF, which the caller has
// and this function does not. So the lane comes from the account's identity
// (realmFromIdentity) and this stays only as the directory-name fallback for an
// account whose identity carries no signal.
func realmFor(root, dirRealm string) string {
	return dirRealm
}

// realmFromIdentity is a FIRST GUESS only. The truth comes from ProbeRealm.
//
// A phone number does NOT mean CN: an overseas z.ai account signs in with a
// phone number too, and treating "has a phone" as "is CN" is what imported a
// fresh overseas z.ai account as cn on 2026-11-25 (the operator had just signed
// in an overseas account and the panel reported it as a domestic one).
//
// What is left here is only a weak prior, used when the probe cannot answer:
// an account carrying an email is more likely overseas, and anything else keeps
// the caller's fallback. Never present this as a decided lane — ProbeRealm is.
func realmFromIdentity(phone, email, token string, fallback string) string {
	if e := strings.TrimSpace(email); strings.Contains(e, "@") {
		return realmOverseas
	}
	return fallback
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
	Phone        string `json:"phone"`
}

// ImportedAccount is one account found on this machine.
type ImportedAccount struct {
	Account *model.Account
	Realm   string
	Source  string // which client directory it came from
	Email   string
	// DeviceID is the device identity the server recorded for this account's
	// sign-in, read from its own token. Two accounts sharing one device id are
	// one device to the server, which is what zeroes out new-user entitlements
	// for the second one (apmengzi/autoclaw-switch documents the same trap).
	DeviceID string
	// AltIDs are the account's OTHER stable identifiers. The official client
	// records the same person under two id systems — a UUID-shaped accountId
	// and a numeric userId — and the SMS login path stores the UUID while the
	// JWT carries the number. Importing without knowing both would register
	// one person twice (field: 2026-10-24, autoclaw:153f2fb1… and
	// autoclaw:880098 were the same account).
	AltIDs []string
}

// SharedDeviceIDs reports device identities used by more than one imported
// account, keyed by device id, with the account ids that share it.
//
// This is how "sign in one account, import it, sign in the next, import that"
// is kept honest: each sign-in is SUPPOSED to mint a fresh device identity, so
// a collision means two accounts will be seen by the server as one machine —
// which costs the second one its new-user grants. The import still goes
// through (the credentials are valid either way); this is the warning.
func SharedDeviceIDs(accounts []ImportedAccount) map[string][]string {
	byDevice := map[string][]string{}
	for _, ia := range accounts {
		id := strings.TrimSpace(ia.DeviceID)
		if id == "" || ia.Account == nil {
			continue
		}
		byDevice[id] = append(byDevice[id], ia.Account.ID)
	}
	for id, owners := range byDevice {
		if len(owners) < 2 {
			delete(byDevice, id)
		}
	}
	return byDevice
}

// ImportFromLocalClients scans the official clients' credential stores and
// returns every account it can decrypt.
//
// A directory that does not exist is skipped silently (that client is simply
// not installed); a directory that exists but yields nothing is reported, since
// "installed but not signed in" is a state the operator needs to understand.
func ImportFromLocalClients() (accounts []ImportedAccount, notes []string) {
	return importFromClientDirs(clientDirs)
}

// ImportWithProbe is ImportFromLocalClients, then asks the upstream which lane
// each account is really in.
//
// This exists because every local signal is ambiguous on this machine: there is
// only ONE client directory (AutoClaw-official) and it holds CN phone accounts,
// channel.json is a release channel rather than a region, and an overseas z.ai
// account signs in with a phone number just like a CN one. The one thing that
// cannot lie is which host accepts the credential — the two lanes are separate
// credential universes and each refuses the other's token.
//
// Accounts are returned with Realm rewritten to whatever the probe found, and
// their creds blob's Region updated to match, so a later call takes the right
// host without probing again. A probe that cannot answer (network down, neither
// host replying) keeps the identity-based guess and says so in the notes: that
// is honest and recoverable, where silently guessing is not.
func ImportWithProbe(ctx context.Context, d *Driver) (accounts []ImportedAccount, notes []string) {
	accounts, notes = ImportFromLocalClients()
	for i := range accounts {
		ia := &accounts[i]
		cr, err := ParseCreds(ia.Account.CredsJSON)
		if err != nil {
			notes = append(notes, ia.Account.Nickname+": 凭据解析失败，线路未核实")
			continue
		}
		realm, err := d.ProbeRealm(ctx, cr)
		if err != nil {
			notes = append(notes, ia.Account.Nickname+": 线路未核实（"+err.Error()+"），暂按 "+ia.Realm+" 处理")
			continue
		}
		if realm != ia.Realm {
			notes = append(notes, ia.Account.Nickname+": 线路按上游实测更正为 "+realm+"（本地判断为 "+ia.Realm+"）")
			ia.Realm = realm
			ia.Account.Realm = realm
			cr.Region = realm
			ia.Account.CredsJSON = cr.Marshal()
		}
	}
	return accounts, notes
}

// importFromClientDirs is ImportFromLocalClients with the directory list
// injected, so a test can scan a temp tree and still exercise the real
// "marker decides the lane" wiring rather than only realmFor in isolation.
func importFromClientDirs(dirs []struct {
	dir   string
	realm string
}) (accounts []ImportedAccount, notes []string) {
	for _, cd := range dirs {
		root, err := clientDataDir(cd.dir)
		if err != nil {
			notes = append(notes, cd.dir+": "+err.Error())
			continue
		}
		if _, err := os.Stat(root); err != nil {
			notes = append(notes, cd.dir+": 未安装（目录不存在）")
			continue
		}
		// The lane comes from the ACCOUNT's identity (a phone means CN, an email
		// means overseas), not from the directory name and not from
		// channel.json — the latter is a release channel, not a region. The
		// directory name is only the fallback when identity says nothing.
		// See realmFromIdentity for why.
		baseRealm := realmFor(root, cd.realm)
		found, err := importClientRoot(root, baseRealm)
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

// importClientRoot scans one already-resolved client root under the given lane.
//
// Split out so the lane decision and the directory walk can be tested together
// without a fake %APPDATA% and without DPAPI: the test fixes the realm and
// checks the accounts that come out are stamped with it. A test of realmFor
// alone would stay green if the caller stopped calling it — this is the seam
// that catches that.
func importClientRoot(root, realm string) ([]ImportedAccount, error) {
	if realm == "" {
		realm = realmFor(root, "")
	}
	if observeImportRealm != nil {
		observeImportRealm(realm)
	}
	return importFromClientDir(root, realm)
}

// observeImportRealm is a test seam: it reports the lane the scan decided to
// walk a client store under. Nil in production. It exists because the store
// needs the real Windows DPAPI key to open, so a test cannot see the imported
// account — but "did the scan use the marker's answer" is the defect being
// guarded and it is observable one step earlier.
var observeImportRealm func(realm string)

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
		if ia, ok := accountFromCredential(cred, prof, dir, realm); ok {
			out = append(out, ia)
		}
	}
	return out, nil
}

// accountFromCredential turns one decrypted credential into an account.
//
// The lane is decided HERE, from the account's own identity, because that is
// the only thing that actually distinguishes the two credential universes: a CN
// account signed in with a phone number, an overseas one with Google or z.ai
// (an email). `fallbackRealm` is the directory-name answer, used only when the
// identity carries neither signal.
//
// Split out from the directory walk so the lane a decryptable account ends up
// with is testable without DPAPI: the walk needs the real Windows user to
// unwrap the store key, but "which lane did this account end up in" is plain
// code, and it is where the field bug lived.
func accountFromCredential(cred *importedCredential, prof profileFields, source, fallbackRealm string) (ImportedAccount, bool) {
	uid := firstNonEmpty(cred.UserID, prof.NumericUserID, prof.AccountID)
	if uid == "" {
		return ImportedAccount{}, false
	}
	token := strings.TrimSpace(cred.AccessToken)
	if token == "" {
		return ImportedAccount{}, false
	}
	if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = "Bearer " + token
	}
	// The device identity the SERVER recorded for this sign-in. The client's own
	// credential field is preferred when present; otherwise read the claim out
	// of the token, which always carries it. Never invent one here: a made-up id
	// breaks the refresh path, and a shared one makes two accounts one device.
	deviceID := firstNonEmpty(
		strings.TrimSpace(cred.DeviceID),
		deviceIDFromTokens(token, cred.RefreshToken),
	)
	// The lane, from the account's own identity. The profile is the plaintext
	// half and always has the phone for a CN account; the credential blob
	// carries the email for an overseas one.
	phone := firstNonEmpty(phoneFromMask(prof), cred.Phone)
	email := firstNonEmpty(cred.Email, prof.Email)
	realm := realmFromIdentity(phone, email, token, fallbackRealm)
	cr := &Creds{
		AccessToken:  token,
		RefreshToken: cred.RefreshToken,
		DeviceID:     deviceID,
		UID:          uid,
		Phone:        phone,
		Region:       realm,
	}
	var alt []string
	for _, id := range []string{cred.UserID, prof.NumericUserID, prof.AccountID} {
		if id != "" && id != uid {
			alt = append(alt, id)
		}
	}
	return ImportedAccount{
		Realm:    realm,
		Source:   source,
		AltIDs:   alt,
		Email:    firstNonEmpty(cred.Email, prof.Email),
		DeviceID: deviceID,
		Account: &model.Account{
			ID:        model.ChannelAutoClaw + ":" + uid,
			Channel:   model.ChannelAutoClaw,
			UID:       uid,
			Nickname:  firstNonEmpty(cred.Nickname, prof.DisplayName, cred.Email, prof.Email, uid),
			Realm:     realm,
			CredsJSON: cr.Marshal(),
		},
	}, true
}

// profileFields is the plaintext half of the client's account folder.
type profileFields struct {
	NumericUserID string
	AccountID     string
	DisplayName   string
	Email         string
	// Phone is the masked form the client stores ("181****7452"). Its PRESENCE
	// is the signal that matters: only a CN account signed in with a number
	// carries one, so it is what decides the lane (see realmFromIdentity).
	Phone string
}

// phoneFromMask returns the masked phone the profile recorded, or "" when it
// has none. The value is never used as a credential — readability is not the
// point, "was this a phone sign-in" is.
func phoneFromMask(p profileFields) string {
	return strings.TrimSpace(p.Phone)
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
			Phone         string `json:"phone"`
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
		Phone:         p.Phone,
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

// decryptBlob opens a Chromium "v10" blob: 3-byte magic, 12-byte nonce,
// AES-256-GCM ciphertext with the tag appended. It returns the plaintext, which
// the two client stores wrap differently (a JSON credential object domestically,
// a bare token string overseas).
func decryptBlob(key, blob []byte) ([]byte, error) {
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
	return plain, nil
}

// decryptCredentialsBlob opens the DOMESTIC store's blob, which holds a JSON
// credential object.
func decryptCredentialsBlob(key, blob []byte) (*importedCredential, error) {
	plain, err := decryptBlob(key, blob)
	if err != nil {
		return nil, err
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
