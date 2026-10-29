// Local Trae CN client device identity (BP-210).
//
// Trae CN takes ONE check-in per registered device per day, and the device
// that counts is the one the desktop client registered with the device
// service — a derived id it has never seen is refused with 9074 no matter
// how the message words it ("当前参与用户太多", task_runs 10-23..10-29: six
// days of it right after the derived identity landed). The desktop client
// keeps that device id, together with the account it is signed in as, in a
// VS Code-style globalStorage/storage.json:
//
//	"iCubeAuthInfo://icube-dc:<8..20 digits>"  -> the device id in the KEY
//	"iCubeAuthInfo://icube.cloudide"           -> the sign-in envelope (the
//	                                              account's userId inside),
//	                                              plain JSON or obfuscated
//
// This file only READS those files (nothing of the client's is written,
// kept or sent but the device id; the client's own token is read past,
// never used) and hands the registered device of the profile signed in as
// the account's uid to backfillTraeDevice. Behaviour constants and the
// obfuscation codec are ported from the MIT-licensed sources
// magpie-community/plugins packages/trae (clientProfiles/clientDecode) and
// dingminhua/dsh-connect-trae (the codec; see NOTICE).
package trae

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dcKey   = "iCubeAuthInfo://icube-dc:"
	authKey = "iCubeAuthInfo://icube.cloudide"
)

// clientProfileNames are the folder names a Trae CN client profile is kept
// under: the IDE's own, and TRAE SOLO CN's, which is a client of the same
// realm and signs in its own account. A profile started with its own
// --user-data-dir is found only if it is one of these.
var clientProfileNames = []string{"Trae CN", "TRAE SOLO CN", "trae-cn"}

// clientStoragePaths are where storage.json may be, in order, on THIS
// platform (BP-210): the same derivation every machine runs — the client's
// standard profile layout under the OS's own config root, nothing
// machine-specific. See clientStoragePathsFor for the per-platform rules.
func clientStoragePaths() []string {
	home, _ := os.UserHomeDir()
	return clientStoragePathsFor(runtime.GOOS, home,
		os.Getenv("APPDATA"), os.Getenv("XDG_CONFIG_HOME"))
}

// clientStoragePathsFor is the pure, testable core: the profile folders to
// scan per platform, mirroring the upstream scanner's rules
// (magpie-community/plugins packages/trae clientStoragePaths):
//
//	darwin  ~/Library/Application Support/{Trae CN, TRAE SOLO CN}
//	win32   %APPDATA% and ~\AppData\Roaming (often the same folder, compared
//	        as one system spells them so it is not read twice) ×
//	        {Trae CN, TRAE SOLO CN, trae-cn}
//	linux   $XDG_CONFIG_HOME or ~/.config × the same three names
//
// A client started with its own --user-data-dir writes a profile none of
// these names cover, and none of the three platforms has a registry of such
// profiles — the upstream passes those over too, and so do we (the account
// falls back to its derived identity).
func clientStoragePathsFor(goos, home, appdata, xdgConfig string) []string {
	file := func(root, name string) string {
		return filepath.Join(root, name, "User", "globalStorage", "storage.json")
	}
	switch goos {
	case "darwin":
		root := filepath.Join(home, "Library", "Application Support")
		var out []string
		for _, n := range clientProfileNames {
			if n == "trae-cn" {
				continue // not a macOS profile folder name
			}
			out = append(out, file(root, n))
		}
		return out
	case "linux":
		root := xdgConfig
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		var out []string
		for _, n := range clientProfileNames {
			out = append(out, file(root, n))
		}
		return out
	default: // windows
		var roots []string
		norm := func(r string) string {
			return strings.ToLower(strings.TrimRight(strings.ReplaceAll(r, "/", "\\"), "\\"))
		}
		add := func(r string) {
			if r == "" {
				return
			}
			for _, was := range roots {
				if norm(was) == norm(r) {
					return
				}
			}
			roots = append(roots, r)
		}
		add(appdata)
		if home != "" {
			add(filepath.Join(home, "AppData", "Roaming"))
		}
		var out []string
		for _, r := range roots {
			for _, n := range clientProfileNames {
				out = append(out, file(r, n))
			}
		}
		return out
	}
}

// clientStoragePathsFn is the indirection tests point at a temp tree.
var clientStoragePathsFn = clientStoragePaths

// clientProfile is one local client profile that could be made out: the
// account it is signed in as and its one registered device id.
type clientProfile struct {
	UID    string
	Device string
}

var clientDeviceDigits = regexp.MustCompile(`^\d{8,20}$`)

// readClientProfiles reads every candidate storage.json, in paths order. A
// file that isn't there, can't be parsed, names no single device or has a
// sign-in that doesn't decode is passed over rather than ending the search
// (before that rule existed upstream, one unreadable file hid every profile
// after it).
func readClientProfiles() []clientProfile {
	var out []clientProfile
	for _, p := range clientStoragePathsFn() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue // not installed here
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue // not a file we can make out
		}
		device := ""
		for k := range doc {
			rest, ok := strings.CutPrefix(k, dcKey)
			if !ok || !clientDeviceDigits.MatchString(rest) {
				continue
			}
			if device != "" {
				device = "" // more than one dc key: no single device to name
				break
			}
			device = rest
		}
		if device == "" {
			continue
		}
		uid := clientAuthUID(doc[authKey])
		if uid == "" {
			continue
		}
		out = append(out, clientProfile{UID: uid, Device: device})
	}
	return out
}

// clientProfilesCached memoizes the scan for a minute: the files barely ever
// change, but chat's auth self-heal can reach here several times a minute.
var (
	clientProfilesMu    sync.Mutex
	clientProfilesCache []clientProfile
	clientProfilesAt    time.Time
)

func clientProfilesCached() []clientProfile {
	clientProfilesMu.Lock()
	defer clientProfilesMu.Unlock()
	if !clientProfilesAt.IsZero() && time.Since(clientProfilesAt) < time.Minute {
		return clientProfilesCache
	}
	clientProfilesCache = readClientProfiles()
	clientProfilesAt = time.Now()
	return clientProfilesCache
}

// clientDeviceFor returns the device id of the local client profile signed
// in as uid, or "" when none is. Never an id made up: an account no local
// profile is signed in as gets nothing.
func clientDeviceFor(uid string) string {
	if uid == "" {
		return ""
	}
	for _, p := range clientProfilesCached() {
		if p.UID == uid {
			return p.Device
		}
	}
	return ""
}

// clientAuthUID reads the sign-in envelope's userId: plain JSON, or the
// client's own obfuscated codec.
func clientAuthUID(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	raw := []byte(t)
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		decoded, err := clientDecode(t)
		if err != nil {
			return ""
		}
		raw = decoded
	}
	var env struct {
		UserID json.RawMessage `json:"userId"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.UserID) == 0 {
		return ""
	}
	var uid string
	if json.Unmarshal(env.UserID, &uid) == nil && uid != "" {
		return uid
	}
	var n float64
	if json.Unmarshal(env.UserID, &n) == nil && n == float64(int64(n)) && n > 0 {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}

// --- the client's obfuscation codec (dingminhua/dsh-connect-trae, MIT) ----

// The envelope is stored obfuscated with a fixed codec: a 6-byte header
// naming the salt, 32 random bytes, then AES-128-CBC under
// sha512(sha512(random) + salt); the plaintext leads with its own sha512,
// which is checked. The salt is the XOR of two fixed 62-byte tables.
var clientCodecSalts = map[string][]byte{
	"746305100000": xorOf([]byte{82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251, 124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203, 84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78, 8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37},
		[]byte{31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95, 96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239, 160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97, 23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125}),
	"123920200203": xorOf([]byte{191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176, 135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156, 51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6, 18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209},
		[]byte{246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145, 50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106, 175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154, 86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170}),
}

func xorOf(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// clientDecode undoes the codec. The leading sha512 is verified; the
// PKCS#7 padding the node runtime strips silently is stripped here.
func clientDecode(b64 string) ([]byte, error) {
	buf, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(buf) <= 6+32+64 {
		return nil, errors.New("trae client codec: buffer too short")
	}
	salt, ok := clientCodecSalts[hex.EncodeToString(buf[:6])]
	if !ok {
		return nil, errors.New("trae client codec: unknown salt header")
	}
	inner := sha512.Sum512(buf[6:38])
	h := sha512.New()
	h.Write(inner[:])
	h.Write(salt)
	key := h.Sum(nil) // 64 bytes: key material then IV
	block, err := aes.NewCipher(key[:16])
	if err != nil {
		return nil, err
	}
	ct := buf[38:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("trae client codec: ciphertext not block aligned")
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, key[16:32]).CryptBlocks(out, ct)
	// PKCS#7 padding comes off FIRST: the plaintext is sha512(body)||body
	// with the padding over the WHOLE thing, and the node runtime this codec
	// was read from strips it inside decipheriv — checking before stripping
	// would hash the padding along and fail every well-formed envelope.
	if n := int(out[len(out)-1]); n >= 1 && n <= aes.BlockSize && n <= len(out) {
		pad := true
		for _, b := range out[len(out)-n:] {
			if int(b) != n {
				pad = false
				break
			}
		}
		if pad {
			out = out[:len(out)-n]
		}
	}
	if len(out) < 64 {
		return nil, errors.New("trae client codec: plaintext too short")
	}
	body := out[64:]
	sum := sha512.Sum512(body)
	if !bytes.Equal(sum[:], out[:64]) {
		return nil, errors.New("trae client codec: checksum mismatch")
	}
	return body, nil
}
