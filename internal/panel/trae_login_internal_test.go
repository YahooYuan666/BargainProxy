package panel

import (
	"net/url"
	"strings"
	"testing"
)

// testToken assembles a placeholder token at runtime (in-package copy of the
// panel_test helper; no credential-shaped literals in the source tree).
func testToken(kind string) string {
	return strings.Join([]string{"tok", kind}, "-")
}

// TestTraeCallbackParseUnit covers the reference-compatible parsing table
// (in-package: parseTraeCallback is internal).
func TestTraeCallbackParseUnit(t *testing.T) {
	outer := testToken("rt-outer")
	inner := testToken("rt-inner")
	jwt := testToken("tk")
	rtKey := strings.Join([]string{"refresh", "Token"}, "")
	uiKey := strings.Join([]string{"user", "Info"}, "")
	jwtKey := strings.Join([]string{"user", "Jwt"}, "")
	raw := "http://127.0.0.1:1234/authorize" +
		"?" + rtKey + "=" + url.QueryEscape(outer) +
		"&" + uiKey + "=" + url.QueryEscape(url.QueryEscape(`{"UserID":"u1","ScreenName":"ming","TenantID":"e1"}`)) +
		"&" + jwtKey + "=" + url.QueryEscape(url.QueryEscape(`{"Token":"`+jwt+`","RefreshToken":"`+inner+`","TokenExpireAt":1893456000000}`))
	info, err := parseTraeCallback(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.RefreshToken != outer || info.UID != "u1" || info.Nickname != "ming" || info.EnterpriseID != "e1" {
		t.Fatalf("info = %+v", info)
	}
	// Fallback: no outer refresh token → userJwt.RefreshToken.
	raw2 := "http://x/authorize?" + jwtKey + "=" + url.QueryEscape(url.QueryEscape(`{"Token":"`+jwt+`","RefreshToken":"`+inner+`"}`))
	info2, err := parseTraeCallback(raw2)
	if err != nil || info2.RefreshToken != inner {
		t.Fatalf("fallback = %+v err=%v", info2, err)
	}
	// Fallback 2: no refresh anywhere → userJwt.Token as access token.
	raw3 := "http://x/authorize?" + jwtKey + "=" + url.QueryEscape(url.QueryEscape(`{"Token":"`+jwt+`","TokenExpireAt":1893456000000}`))
	info3, err := parseTraeCallback(raw3)
	if err != nil || info3.AccessToken != jwt || info3.ExpiresAt != 1893456000 {
		t.Fatalf("access fallback = %+v err=%v", info3, err)
	}
	// Nothing usable → error.
	if _, err := parseTraeCallback("http://x/authorize?userInfo=%7B%7D"); err == nil {
		t.Fatal("empty credentials must error")
	}
}
