package autoclaw

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// ProbeRealm asks the two hosts which one this credential belongs to.
//
// The lane cannot be read off the directory name (there is only ever one
// client directory on this machine: AutoClaw-official, and it holds CN phone
// accounts), and it cannot be read off channel.json (that is a release channel:
// official/production/test/pre). The account's identity helps but is not
// decisive either — an overseas z.ai account signs in with a phone number too.
//
// What IS decisive is which host accepts the credential. The two lanes are
// separate credential universes: a CN token gets 401 from the overseas host and
// vice versa (measured 2026-10-24 and again 2026-11-25). So ask the host.
//
// A cheap identity route is used (/userapi/v1/user-info), not a model call: this
// must not spend credits. Nothing is cached: it is one request per import, and a
// wrong answer is worse than the request.
func (d *Driver) ProbeRealm(ctx context.Context, cr *Creds) (string, error) {
	if cr == nil || strings.TrimSpace(cr.AccessToken) == "" {
		return "", fmt.Errorf("autoclaw probe: no token")
	}
	// Ask the overseas host first: this is only ever called for an account whose
	// lane is unknown, and a CN account is the case we most need to get right
	// (importing a CN account as overseas spends a doomed overseas attempt on
	// every request).
	if d.realmAnswers(ctx, cr, true) {
		return realmOverseas, nil
	}
	if d.realmAnswers(ctx, cr, false) {
		return "cn", nil
	}
	return "", fmt.Errorf("autoclaw probe: neither host accepted this credential")
}

// realmAnswers reports whether the given lane's identity API takes the token.
func (d *Driver) realmAnswers(ctx context.Context, cr *Creds, overseas bool) bool {
	err := d.userapiCallFor(ctx, http.MethodPost, "/userapi/v1/user-info",
		cr.bearer(), map[string]any{"source_id": "autoclaw", "device_id": cr.DeviceID}, nil, overseas)
	if err == nil {
		return true
	}
	// A 401/403 from a host is a definite "not this lane". Anything else
	// (transport, 5xx) is inconclusive, and treating it as a "no" would
	// misclassify an account on a flaky moment — so only an explicit auth
	// refusal counts as an answer.
	s := strings.ToLower(err.Error())
	return !(strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "invalid token") || strings.Contains(s, "session dead") ||
		strings.Contains(s, "未登录") || strings.Contains(s, "log in"))
}
