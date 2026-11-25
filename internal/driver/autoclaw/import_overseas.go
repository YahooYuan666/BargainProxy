// Reader for the OVERSEAS client's sign-in store.
//
// The two clients keep their sign-in states in different places and different
// shapes, and using the wrong one is what made the panel's "import" button put a
// DOMESTIC account where an overseas one was expected (field 2026-11-25):
//
//	overseas (AutoClawGlobal, 1.x shape)  %APPDATA%\AutoClaw\auth.json
//	  { deviceId, token: "enc:v10…", refreshToken: "enc:v10…",
//	    userInfo: { user_id: "<uuid>", user_name: "…" }, updatedAt }
//
//	domestic (AutoClaw2, 2.x shape)       %APPDATA%\AutoClaw-official\accounts\<hash>\
//	  account-credentials.enc             (v10, no "enc:" prefix)
//
// The encryption is the same Chromium os_crypt scheme in both, but the key comes
// from each directory's own Local State, so the overseas store must be decrypted
// with the overseas key. Everything else in this file (DPAPI, AES-GCM) is shared
// with the domestic reader.
package autoclaw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bargainproxy/internal/model"
)

// overseasClientDir is where the overseas client writes its sign-in state.
const overseasClientDir = "AutoClaw"

// overseasAuth is the subset of auth.json this driver needs.
type overseasAuth struct {
	DeviceID     string `json:"deviceId"`
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	UpdatedAt    int64  `json:"updatedAt"`
	UserInfo     struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		ID       any    `json:"id"`
	} `json:"userInfo"`
	User             string `json:"user"`
	Email            string `json:"email"`
	UserIDDirect     string `json:"user_id"`
	UserNameDirect   string `json:"user_name"`
}

// ImportOverseas reads the overseas client's sign-in as ONE account.
//
// One account, not many: that store holds a single sign-in (last one wins), which
// is exactly why adding a second overseas account means signing in again. The
// caller imports, then signs in the next one, then imports again — the previous
// account's already-copied credentials stay valid because importing only reads
// this file and never calls logout.
func ImportOverseas() ([]ImportedAccount, []string) {
	root, err := clientDataDir(overseasClientDir)
	if err != nil {
		return nil, []string{"海外版: " + err.Error()}
	}
	authPath := filepath.Join(root, "auth.json")
	raw, err := os.ReadFile(authPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []string{"海外版: 未登录（" + authPath + " 不存在）"}
		}
		return nil, []string{"海外版: " + err.Error()}
	}
	var a overseasAuth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, []string{"海外版: auth.json 解析失败: " + err.Error()}
	}
	if strings.TrimSpace(a.Token) == "" {
		return nil, []string{"海外版: auth.json 里没有 token"}
	}

	key, err := osCryptKey(root)
	if err != nil {
		return nil, []string{"海外版: 读不到解密密钥（" + err.Error() + "）"}
	}
	access, err := decryptEnvelope(key, a.Token)
	if err != nil {
		return nil, []string{"海外版: token 解密失败（" + err.Error() + "）"}
	}
	var refresh string
	if a.RefreshToken != "" {
		if r, err := decryptEnvelope(key, a.RefreshToken); err == nil {
			refresh = r
		}
	}

	uid := firstNonEmpty(a.UserInfo.UserID, a.UserIDDirect)
	name := firstNonEmpty(a.UserInfo.UserName, a.UserNameDirect, a.User, a.Email)
	if uid == "" {
		return nil, []string{"海外版: auth.json 里没有 user_id"}
	}
	token := access
	if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = "Bearer " + token
	}

	cr := &Creds{
		AccessToken:  token,
		RefreshToken: refresh,
		// The device id this store records is the one the SERVER issued; taking it
		// keeps the refresh path signing with the same identity.
		DeviceID: firstNonEmpty(strings.TrimSpace(a.DeviceID), deviceIDFromTokens(token, refresh)),
		UID:      uid,
		UserName: name,
		Region:   realmOverseas,
	}
	return []ImportedAccount{{
		Realm:    realmOverseas,
		Source:   authPath,
		Email:    firstNonEmpty(a.Email, name),
		DeviceID: cr.DeviceID,
		Account: &model.Account{
			ID:        model.ChannelAutoClaw + ":" + uid,
			Channel:   model.ChannelAutoClaw,
			UID:       uid,
			Nickname:  firstNonEmpty(name, uid),
			Realm:     realmOverseas,
			CredsJSON: cr.Marshal(),
		},
	}}, nil
}

// decryptEnvelope opens a value that may carry the "enc:" prefix the overseas
// store uses, or be bare base64 of the v10 blob. The plaintext is a bare token
// string here (the domestic store wraps a JSON object instead), so it is taken
// as-is with any surrounding quotes trimmed.
func decryptEnvelope(key []byte, v string) (string, error) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "enc:")
	blob, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	plain, err := decryptBlob(key, blob)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(string(plain))
	out = strings.Trim(out, `"`)
	if out == "" {
		return "", fmt.Errorf("解密结果为空")
	}
	return out, nil
}

// ImportOverseasWithProbe reads the overseas store and then confirms the lane
// with the upstream, so a store that has been re-signed into a different
// account cannot smuggle the wrong lane through.
func ImportOverseasWithProbe(ctx context.Context, d *Driver) ([]ImportedAccount, []string) {
	accounts, notes := ImportOverseas()
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
			notes = append(notes, ia.Account.Nickname+": 上游实测此号属于 "+realm+"，不是海外号")
			ia.Realm = realm
			ia.Account.Realm = realm
			cr.Region = realm
			ia.Account.CredsJSON = cr.Marshal()
		}
	}
	return accounts, notes
}
