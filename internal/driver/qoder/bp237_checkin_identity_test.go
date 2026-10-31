package qoder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// BP-237 red-first tests (2026-11-01).
//
// FIELD BUG: the task ledger said "qoder checkin not available" every day for
// two weeks. Read live on 2026-11-01, the same account answered two different
// campaigns documents depending ONLY on the device identity in the request:
//
//	derived identity (uid+salt md5/sha512): 200 with NO CLAIM_BENEFIT campaign
//	Qoder's own runtime-info identity:      200 with
//	  {"actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
//	   "benefit":{"kind":"CREDITS","amount":100,...}}
//
// So the daily credits are offered only to a device Qoder itself registered
// (magpie-community/plugins packages/qoder 0.2.50 says the same). The check-in
// path must therefore read the real identity from Qoder's signed helper, with
// a hand-pinned pair and the derived identity as fallbacks.

// fakeRuntimeInfo writes an executable stand-in for Qoder's runtime-info.exe
// that prints the given identity. It is a shell script on unix and a .bat on
// windows; the driver runs it as an external command, so the shape that
// matters is stdin + argv, not the binary.
func fakeRuntimeInfo(t *testing.T, token, typ string) string {
	t.Helper()
	dir := t.TempDir()
	if isWindows() {
		p := filepath.Join(dir, "runtime-info.bat")
		body := "@echo off\r\nset /p ACCOUNT=\r\n" +
			`echo {"machineToken":"` + token + `","machineType":"` + typ + `"}` + "\r\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("write fake runtime-info: %v", err)
		}
		return p
	}
	p := filepath.Join(dir, "runtime-info")
	body := "#!/bin/sh\nread -r ACCOUNT\n" +
		`echo '{"machineToken":"` + token + `","machineType":"` + typ + `"}'` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake runtime-info: %v", err)
	}
	return p
}

// campaignsServer answers the check-in path, recording the identity headers of
// every campaigns request. The legacy status always says DISABLED so the
// campaigns path is the one under test.
type campaignsServer struct {
	*httptest.Server
	tokens []string
	types  []string
}

func newCampaignsServer(t *testing.T, campaignsBody string) *campaignsServer {
	t.Helper()
	cs := &campaignsServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			cs.tokens = append(cs.tokens, r.Header.Get("Cosy-MachineToken"))
			cs.types = append(cs.types, r.Header.Get("Cosy-MachineType"))
			fmt.Fprint(w, campaignsBody)
		case strings.Contains(r.URL.Path, "/campaigns/") && strings.HasSuffix(r.URL.Path, "/claim"):
			fmt.Fprint(w, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

// testQoderAccountWithUID builds an account that carries a uid, so the
// device-identity helper (which keys on the uid) can be exercised.
func testQoderAccountWithUID(t *testing.T) *model.Account {
	t.Helper()
	acc := testQoderAccount(t)
	acc.UID = "uid-237"
	return acc
}

// TestCheckinUsesRuntimeInfoIdentity: with a helper present, the campaigns
// request must carry ITS identity, not the derived one. This is the whole fix:
// the derived identity is what makes the daily campaign vanish.
func TestCheckinUsesRuntimeInfoIdentity(t *testing.T) {
	const realToken, realType = "REAL-TOKEN-from-qoder-helper", "REAL-TYPE"
	exe := fakeRuntimeInfo(t, realToken, realType)
	cs := newCampaignsServer(t, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`)

	cfg := config.QoderSettings{GatewayBase: cs.URL, OpenBase: cs.URL, CheckinRuntimeInfo: exe}
	d := New(cfg, nil)
	checkedIn, credits, enable, err := d.Checkin(context.Background(), testQoderAccountWithUID(t))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if len(cs.tokens) == 0 {
		t.Fatal("the campaigns list was never requested")
	}
	if got := cs.tokens[0]; got != realToken {
		t.Fatalf("Cosy-MachineToken = %q, want the runtime-info identity %q", got, realToken)
	}
	if got := cs.types[0]; got != realType {
		t.Fatalf("Cosy-MachineType = %q, want %q", got, realType)
	}
	if checkedIn || credits != 100 || !enable {
		t.Fatalf("result = (%v, %d, %v), want (false, 100, true)", checkedIn, credits, enable)
	}
}

// TestCheckinIdentityPrefersConfigWhenHelperMissing: no helper on the machine,
// but the operator pinned a pair — that pair must be sent (and must NOT be
// silently replaced by the derived identity).
func TestCheckinIdentityPrefersConfigWhenHelperMissing(t *testing.T) {
	cs := newCampaignsServer(t, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`)
	cfg := config.QoderSettings{
		GatewayBase: cs.URL, OpenBase: cs.URL,
		CheckinRuntimeInfo:  filepath.Join(t.TempDir(), "absent-runtime-info"),
		CheckinMachineToken: "PINNED-TOKEN",
		CheckinMachineType:  "PINNED-TYPE",
	}
	d := New(cfg, nil)
	if _, _, _, err := d.Checkin(context.Background(), testQoderAccount(t)); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if len(cs.tokens) == 0 {
		t.Fatal("the campaigns list was never requested")
	}
	if cs.tokens[0] != "PINNED-TOKEN" || cs.types[0] != "PINNED-TYPE" {
		t.Fatalf("sent (%q, %q), want the pinned pair", cs.tokens[0], cs.types[0])
	}
}

// TestCheckinIdentityFallsBackToDerived: no helper, no pinned pair — the
// derived identity is still sent (the old behaviour), so the check-in degrades
// to "not available" instead of crashing or sending nothing.
func TestCheckinIdentityFallsBackToDerived(t *testing.T) {
	cs := newCampaignsServer(t, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`)
	cfg := config.QoderSettings{
		GatewayBase:        cs.URL,
		OpenBase:           cs.URL,
		CheckinRuntimeInfo: filepath.Join(t.TempDir(), "absent-runtime-info"),
	}
	d := New(cfg, nil)
	acc := testQoderAccount(t)
	if _, _, _, err := d.Checkin(context.Background(), acc); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if len(cs.tokens) == 0 {
		t.Fatal("the campaigns list was never requested")
	}
	wantTok, wantTyp := d.machineIdentityFor(acc.UID)
	if cs.tokens[0] != wantTok || cs.types[0] != wantTyp {
		t.Fatalf("sent (%q, %q), want the derived pair (%q, %q)", cs.tokens[0], cs.types[0], wantTok, wantTyp)
	}
}

// TestCheckinIdentityCachesHelperReads: the helper is an external process; one
// read per TTL, not one per campaigns request.
func TestCheckinIdentityCachesHelperReads(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	exe := filepath.Join(dir, "runtime-info.bat")
	body := "@echo off\r\nset /p ACCOUNT=\r\necho x >> \"" + counter + "\"\r\n" +
		`echo {"machineToken":"TOK","machineType":"TYP"}` + "\r\n"
	if !isWindows() {
		exe = filepath.Join(dir, "runtime-info")
		body = "#!/bin/sh\nread -r ACCOUNT\necho x >> '" + counter + "'\n" +
			`echo '{"machineToken":"TOK","machineType":"TYP"}'` + "\n"
	}
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake runtime-info: %v", err)
	}
	d := New(config.QoderSettings{CheckinRuntimeInfo: exe}, nil)
	acc := testQoderAccountWithUID(t)
	for i := 0; i < 3; i++ {
		if _, _, ok := d.realCheckinIdentity(context.Background(), acc.UID); !ok {
			t.Fatalf("read %d: helper identity not obtained", i)
		}
	}
	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if n := strings.Count(string(raw), "x"); n != 1 {
		t.Fatalf("helper ran %d times, want 1 (cached for %s)", n, checkinIdentityTTL)
	}
}

// TestCheckinIdentityBadHelperOutputFallsBack: a helper that answers garbage
// must not become a header value — it falls through to the next source.
func TestCheckinIdentityBadHelperOutputFallsBack(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "runtime-info.bat")
	body := "@echo off\r\nset /p ACCOUNT=\r\necho not-json\r\n"
	if !isWindows() {
		exe = filepath.Join(dir, "runtime-info")
		body = "#!/bin/sh\nread -r ACCOUNT\necho not-json\n"
	}
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake runtime-info: %v", err)
	}
	cfg := config.QoderSettings{
		CheckinRuntimeInfo:  exe,
		CheckinMachineToken: "PINNED-TOKEN",
		CheckinMachineType:  "PINNED-TYPE",
	}
	d := New(cfg, nil)
	tok, typ, source := d.checkinIdentity(context.Background(), "uid-1")
	if source != "config" {
		t.Fatalf("source = %q, want config (a garbage helper must not be trusted)", source)
	}
	if tok != "PINNED-TOKEN" || typ != "PINNED-TYPE" {
		t.Fatalf("identity = (%q, %q), want the pinned pair", tok, typ)
	}
}

// TestChatHeadersKeepDerivedIdentity pins the SCOPE of the fix: only the
// check-in path may use the real identity. Chat must keep its own derived
// credentials, because rotating them changes every account's chat
// fingerprint.
//
// Note the two derivations differ by seed on purpose: runtimeFor derives from
// the PAT (or account id), machineIdentityFor derives from the uid. The
// assertion is therefore "chat did not take the check-in identity", not "chat
// equals the uid-derived pair".
func TestChatHeadersKeepDerivedIdentity(t *testing.T) {
	const pinToken, pinType = "PINNED-TOKEN", "PINNED-TYPE"
	exe := fakeRuntimeInfo(t, "REAL-TOKEN", "REAL-TYPE")
	cfg := config.QoderSettings{
		CheckinRuntimeInfo:  exe,
		CheckinMachineToken: pinToken,
		CheckinMachineType:  pinType,
	}
	d := New(cfg, nil)
	acc := testQoderAccountWithUID(t)
	rt, err := d.runtimeFor(acc)
	if err != nil {
		t.Fatalf("runtimeFor: %v", err)
	}
	// qoderCommonHeaders is what chat calls sign with.
	if rt.creds.MachineToken == "REAL-TOKEN" || rt.creds.MachineToken == pinToken {
		t.Fatalf("chat credentials took the check-in identity %q — chat must keep its own", rt.creds.MachineToken)
	}
	// And the check-in path really does take the real identity, so the
	// comparison above is not vacuous.
	tok, typ, source := d.checkinIdentity(context.Background(), acc.UID)
	if source != "runtime-info" || tok != "REAL-TOKEN" || typ != "REAL-TYPE" {
		t.Fatalf("check-in identity = (%q, %q, %q), want the runtime-info pair", tok, typ, source)
	}
}

// TestNoCampaignOnDerivedIdentityIsNotSettled is the OTHER half of BP-237: the
// same "campaigns: []" document means two different things, and conflating
// them is what produced two weeks of misleading "not available" rows.
//
//   - derived identity -> Qoder never offered this unknown device anything;
//     actionable, must NOT be a settled ok.
//   - real identity    -> upstream's own answer for this device; settled.
func TestNoCampaignOnDerivedIdentityIsNotSettled(t *testing.T) {
	cs := newCampaignsServer(t, `{"campaigns":[]}`)
	cfg := config.QoderSettings{
		GatewayBase:        cs.URL,
		OpenBase:           cs.URL,
		CheckinRuntimeInfo: filepath.Join(t.TempDir(), "absent-runtime-info"),
	}
	d := New(cfg, nil)
	_, _, enable, err := d.Checkin(context.Background(), testQoderAccountWithUID(t))
	if err == nil {
		t.Fatalf("an empty campaigns list on the derived identity was reported as settled (enable=%v) - that is exactly the two-week false 'not available'", enable)
	}
	if !strings.Contains(err.Error(), "device identity") {
		t.Fatalf("the error must name the device identity as the cause: %v", err)
	}
	if !enable {
		t.Fatal("enable must be true so the task ledger shows a retryable failure, not a settled ok")
	}
}

// TestNoCampaignOnRealIdentityStaysSettled: with a real identity the empty
// list IS the upstream's answer, so it stays a quiet ok.
func TestNoCampaignOnRealIdentityStaysSettled(t *testing.T) {
	cs := newCampaignsServer(t, `{"campaigns":[]}`)
	cfg := config.QoderSettings{
		GatewayBase:         cs.URL,
		OpenBase:            cs.URL,
		CheckinRuntimeInfo:  filepath.Join(t.TempDir(), "absent-runtime-info"),
		CheckinMachineToken: "PINNED-TOKEN",
		CheckinMachineType:  "PINNED-TYPE",
	}
	d := New(cfg, nil)
	_, _, enable, err := d.Checkin(context.Background(), testQoderAccountWithUID(t))
	if err != nil {
		t.Fatalf("a real identity with no campaign must stay a settled ok: %v", err)
	}
	if enable {
		t.Fatal("enable must be false so the task reports 'not available' (settled)")
	}
}

// isWindows reports the test host platform without importing runtime into
// every call site.
func isWindows() bool {
	return os.PathSeparator == '\\'
}

// TestFindQoderRuntimeInfoHonoursConfigPath: an explicit path wins over
// auto-detection, and a path that does not exist yields "" (no panic, no
// accidental execution of something else).
func TestFindQoderRuntimeInfoHonoursConfigPath(t *testing.T) {
	exe := fakeRuntimeInfo(t, "TOK", "TYP")
	if got := findQoderRuntimeInfo(); got != "" && !filepath.IsAbs(got) {
		t.Fatalf("auto-detected path %q is not absolute", got)
	}
	d := New(config.QoderSettings{CheckinRuntimeInfo: exe}, nil)
	tok, typ, source := d.checkinIdentity(context.Background(), "uid-1")
	if source != "runtime-info" {
		t.Fatalf("source = %q, want runtime-info", source)
	}
	if tok != "TOK" || typ != "TYP" {
		t.Fatalf("identity = (%q, %q)", tok, typ)
	}
}

// TestCheckinCampaignsStillParsesBenefitAmount guards the BP-124 reward read
// through the NEW identity path: the credits must still come out of the nested
// benefit.amount.
func TestCheckinCampaignsStillParsesBenefitAmount(t *testing.T) {
	exe := fakeRuntimeInfo(t, "TOK", "TYP")
	cs := newCampaignsServer(t, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}}]}`)
	d := New(config.QoderSettings{GatewayBase: cs.URL, OpenBase: cs.URL, CheckinRuntimeInfo: exe}, nil)
	_, credits, _, err := d.Checkin(context.Background(), testQoderAccount(t))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if credits != 100 {
		t.Fatalf("credits = %d, want 100 (nested benefit.amount)", credits)
	}
}

// TestCheckinClaimsCampaignByCampaignID is the second field bug found on the
// same day: the live campaigns document names the campaign `campaignId`
// (read 2026-11-01), while the code read only `id`. Every real claim therefore
// failed with "CLAIM_BENEFIT campaign without id" — the campaign was present
// and claimable and we refused to name it. Both keys must work.
func TestCheckinClaimsCampaignByCampaignID(t *testing.T) {
	exe := fakeRuntimeInfo(t, "TOK", "TYP")
	var claimedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/status"):
			fmt.Fprint(w, `{"status":"DISABLED"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet:
			// The LIVE shape: campaignId, no id.
			fmt.Fprint(w, `{"claimable":true,"campaigns":[{"campaignId":"01a17b4c-004f-78b7-b635-caca344d4237","campaignKey":"act-20261027-644","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100},"startAt":1793412000,"endAt":1793498340}]}`)
		case strings.Contains(r.URL.Path, "/claim"):
			claimedPath = r.URL.Path
			fmt.Fprint(w, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)
		case strings.HasSuffix(r.URL.Path, "/jobToken/exchange"):
			fmt.Fprintf(w, `{"token":"open-bearer-%d","expires_in":7200}`, time.Now().Unix())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL, CheckinRuntimeInfo: exe}, nil)
	_, credits, enable, err := d.Checkin(context.Background(), testQoderAccountWithUID(t))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if claimedPath == "" {
		t.Fatal("no claim was issued: the campaignId was not read")
	}
	if !strings.Contains(claimedPath, "01a17b4c-004f-78b7-b635-caca344d4237") {
		t.Fatalf("claim path %q does not carry the campaignId", claimedPath)
	}
	if credits != 100 || !enable {
		t.Fatalf("credits=%d enable=%v, want 100/true", credits, enable)
	}
}
