package trae

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// sealClientEnvelope builds a storage.json value in the client's own
// obfuscated codec (the exact inverse of clientDecode), so the tests pin the
// decoder against well-formed input without a real client's secrets.
func sealClientEnvelope(t *testing.T, body []byte, saltHex string) string {
	t.Helper()
	salt, ok := clientCodecSalts[saltHex]
	if !ok {
		t.Fatalf("unknown salt %q", saltHex)
	}
	random := bytes.Repeat([]byte{7}, 32)
	inner := sha512.Sum512(random)
	h := sha512.New()
	h.Write(inner[:])
	h.Write(salt)
	key := h.Sum(nil)
	block, err := aes.NewCipher(key[:16])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(body)
	plain := append(append([]byte{}, sum[:64]...), body...)
	n := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(n)}, n)...)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key[16:32]).CryptBlocks(ct, plain)
	buf := append(append([]byte{}, mustHex(saltHex)...), random...)
	buf = append(buf, ct...)
	return base64.StdEncoding.EncodeToString(buf)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// writeStorage writes one profile's storage.json under a fake client root.
func writeStorage(t *testing.T, root, profile, content string) {
	t.Helper()
	dir := filepath.Join(root, profile, "User", "globalStorage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "storage.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withClientRoots points the scanner at a temp tree and clears the cache.
func withClientRoots(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	orig := clientStoragePathsFn
	clientStoragePathsFn = func() []string {
		return []string{
			filepath.Join(root, "Trae CN", "User", "globalStorage", "storage.json"),
			filepath.Join(root, "TRAE SOLO CN", "User", "globalStorage", "storage.json"),
		}
	}
	clientProfilesMu.Lock()
	clientProfilesCache, clientProfilesAt = nil, time.Time{}
	clientProfilesMu.Unlock()
	t.Cleanup(func() {
		clientStoragePathsFn = orig
		clientProfilesMu.Lock()
		clientProfilesCache, clientProfilesAt = nil, time.Time{}
		clientProfilesMu.Unlock()
	})
	return root
}

// TestBP210PlainEnvelopeIsRead: the storage.json may keep the sign-in as
// plain JSON — the profile's device is matched to its account's uid.
func TestBP210PlainEnvelopeIsRead(t *testing.T) {
	root := withClientRoots(t)
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:2866487253502651": "x",
		"iCubeAuthInfo://icube.cloudide": "{\"userId\":\"1234567890\",\"token\":\"x\"}"
	}`)
	if got := clientDeviceFor("1234567890"); got != "2866487253502651" {
		t.Fatalf("clientDeviceFor = %q, want the profile's registered device", got)
	}
	if got := clientDeviceFor("other-uid"); got != "" {
		t.Fatalf("an account no profile is signed in as must get nothing, got %q", got)
	}
}

// TestBP210ObfuscatedEnvelopeIsDecoded: the codec is undone and the leading
// sha512 verified; a tampered body yields nothing.
func TestBP210ObfuscatedEnvelopeIsDecoded(t *testing.T) {
	root := withClientRoots(t)
	env := sealClientEnvelope(t, []byte(`{"userId":2866487253502651,"token":"x"}`), "746305100000")
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:2866487253502651": "x",
		"iCubeAuthInfo://icube.cloudide": "`+env+`"
	}`)
	if got := clientDeviceFor("2866487253502651"); got != "2866487253502651" {
		t.Fatalf("the obfuscated envelope must decode to the numeric uid, got %q", got)
	}

	// A tampered body fails the leading checksum: the profile is skipped.
	// (The cache is dropped first — otherwise the good envelope read above
	// would answer for the tampered file for a minute.)
	bad, _ := base64.StdEncoding.DecodeString(env)
	bad[len(bad)-3] ^= 0xff
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:2866487253502651": "x",
		"iCubeAuthInfo://icube.cloudide": "`+base64.StdEncoding.EncodeToString(bad)+`"
	}`)
	clientProfilesMu.Lock()
	clientProfilesCache, clientProfilesAt = nil, time.Time{}
	clientProfilesMu.Unlock()
	if got := clientDeviceFor("2866487253502651"); got != "" {
		t.Fatalf("a tampered envelope must be passed over, got %q", got)
	}
}

// TestBP210MultipleProfiles: each profile answers for its own account, and a
// broken one does not hide the next.
func TestBP210MultipleProfiles(t *testing.T) {
	root := withClientRoots(t)
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:1111111111111111": "x",
		"iCubeAuthInfo://icube.cloudide": "{\"userId\":\"u-one\"}"
	}`)
	writeStorage(t, root, "TRAE SOLO CN", "not json at all") // broken: passed over
	if got := clientDeviceFor("u-one"); got != "1111111111111111" {
		t.Fatalf("profile one = %q", got)
	}
	if got := clientDeviceFor("u-two"); got != "" {
		t.Fatalf("the broken profile must not invent a device, got %q", got)
	}
}

// TestBP210TwoDeviceKeysIsNoSignal: a storage.json naming two dc keys has no
// single device to name — passed over.
func TestBP210TwoDeviceKeysIsNoSignal(t *testing.T) {
	root := withClientRoots(t)
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:1111111111111111": "x",
		"iCubeAuthInfo://icube-dc:2222222222222222": "y",
		"iCubeAuthInfo://icube.cloudide": "{\"userId\":\"u-amb\"}"
	}`)
	if got := clientDeviceFor("u-amb"); got != "" {
		t.Fatalf("two dc keys must not answer, got %q", got)
	}
}

// TestBP210BackfillPrefersTheClientDevice: a derived identity is REPLACED by
// the local client's registered one — that replacement is the 9074 fix.
func TestBP210BackfillPrefersTheClientDevice(t *testing.T) {
	root := withClientRoots(t)
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:2866487253502651": "x",
		"iCubeAuthInfo://icube.cloudide": "{\"userId\":\"u-acc\"}"
	}`)
	acc := &model.Account{ID: "trae-bp210", Channel: model.ChannelTrae, CredsJSON: mustJSON(t, map[string]string{"uid": "u-acc", "device_id": "9999999999999999"})}
	backfillTraeDevice(acc)

	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		t.Fatal(err)
	}
	if creds.DeviceID != "2866487253502651" {
		t.Fatalf("device = %q, want the client's registered one replacing the derived value", creds.DeviceID)
	}
	if !acc.CredsDirty {
		t.Fatal("the replacement must be persisted (CredsDirty)")
	}

	// With the client gone, the REAL identity survives — no reverting to a
	// derived one.
	writeStorage(t, root, "Trae CN", "{}")
	clientProfilesMu.Lock()
	clientProfilesCache, clientProfilesAt = nil, time.Time{}
	clientProfilesMu.Unlock()
	backfillTraeDevice(acc)
	creds2, _ := ParseCreds(acc.Credentials())
	if creds2.DeviceID != "2866487253502651" {
		t.Fatalf("a client-less scan must not revert a real identity, got %q", creds2.DeviceID)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestBP210ClientTokenIsNeverTaken: only the device id leaves the file — the
// envelope's token must not end up anywhere (grep the parser's output).
func TestBP210ClientTokenIsNeverTaken(t *testing.T) {
	root := withClientRoots(t)
	writeStorage(t, root, "Trae CN", `{
		"iCubeAuthInfo://icube-dc:1111111111111111": "x",
		"iCubeAuthInfo://icube.cloudide": "{\"userId\":\"u-tok\",\"token\":\"SECRET-TOKEN-VALUE\"}"
	}`)
	acc := &model.Account{ID: "trae-tok", Channel: model.ChannelTrae, CredsJSON: mustJSON(t, map[string]string{"uid": "u-tok"})}
	backfillTraeDevice(acc)
	if strings.Contains(string(acc.Credentials()), "SECRET-TOKEN-VALUE") {
		t.Fatal("the client's own token must be read past, never kept")
	}
}

// TestBP210PathsCoverAllPlatforms pins the per-platform derivation (BP-210):
// the scanner is the same rule set on every machine, keyed off the OS's own
// config roots — nothing machine-specific, and the upstream's exact folder
// names.
func TestBP210PathsCoverAllPlatforms(t *testing.T) {
	const home = `/home/users/dev`
	win := clientStoragePathsFor("windows", `C:\Users\dev`, `C:\Users\dev\AppData\Roaming`, "")
	if len(win) != 3 {
		t.Fatalf("windows roots deduped to %d paths, want 3 (APPDATA == home roaming): %v", len(win), win)
	}
	for _, p := range win {
		if !strings.HasSuffix(filepath.ToSlash(p), "/User/globalStorage/storage.json") {
			t.Fatalf("windows path %q lacks the profile layout", p)
		}
	}
	win2 := clientStoragePathsFor("windows", `C:\Users\dev`, `D:\Other\Roaming`, "")
	if len(win2) != 6 {
		t.Fatalf("two distinct windows roots must yield 6 paths, got %d", len(win2))
	}
	mac := clientStoragePathsFor("darwin", home, "", "")
	if len(mac) != 2 {
		t.Fatalf("darwin must scan 2 profiles (no trae-cn), got %d: %v", len(mac), mac)
	}
	if !strings.Contains(filepath.ToSlash(mac[0]), "Library/Application Support/Trae CN") {
		t.Fatalf("darwin root wrong: %q", mac[0])
	}
	lin := clientStoragePathsFor("linux", home, "", "")
	if len(lin) != 3 || !strings.Contains(filepath.ToSlash(lin[0]), home+"/.config/Trae CN") {
		t.Fatalf("linux default root wrong: %v", lin)
	}
	linX := clientStoragePathsFor("linux", home, "", "/etc/xdg")
	if !strings.HasPrefix(filepath.ToSlash(linX[0]), "/etc/xdg/") {
		t.Fatalf("XDG_CONFIG_HOME must win when set: %q", linX[0])
	}
}
