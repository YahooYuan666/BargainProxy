package panel

import (
	"encoding/json"
	"testing"
)

// TestParseWBAccountRealPayload keeps a regression test for the shape of the
// real international-login response (2026-10-23), with the identity fields
// replaced by same-shaped placeholders.
//
// Why the fields matter (this is what broke): the response wraps the account
// in an "accounts" array, and the fields the panel needs are uid + nickname +
// lastLogin; the extra SSO/oneid/deployStatus objects must not confuse the
// decoder. The repository is public, so a real uid and email address must
// never be committed — the shape is the contract, not the person.
func TestParseWBAccountRealPayload(t *testing.T) {
	const (
		wantUID  = "00000000-0000-4000-8000-000000000000"
		wantNick = "user@example.com"
	)
	raw := json.RawMessage(`{"accounts":[{"uid":"00000000-0000-4000-8000-000000000000","nickname":"user@example.com","uin":"450000000000","type":"personal","lastLogin":true,"isCreator":false,"isAdmin":false,"pluginEnabled":true,"deployStatus":{"statusCode":0,"statusMsg":"","detailMsg":""},"accountType":"","sso":{"domain":"","domainModifiedTimes":0},"idp":"","areaInfoComplete":true,"oneidAccountId":"","isCurrentOneIdEnterprise":false,"isCurrentOneIdPersonal":false,"isFirstLogin":false,"logOutEnable":false}]}`)
	got := parseWBAccount(raw)
	if got.UID != wantUID {
		t.Fatalf("uid = %q, want %q", got.UID, wantUID)
	}
	if got.Nickname != wantNick {
		t.Fatalf("nickname = %q, want %q", got.Nickname, wantNick)
	}
}
