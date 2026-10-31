package qoder

// Daily check-in device identity (BP-237, 2026-11-01).
//
// THE BUG: the task ledger said "qoder checkin not available" every day for
// two weeks. That message is the driver's settled answer for "the campaigns
// page lists NO active CLAIM_BENEFIT campaign" — and it was telling the truth
// about the document it received. Read live on 2026-11-01, the same account
// returned two DIFFERENT campaigns documents depending only on the device
// identity in the request:
//
//	derived identity (md5/sha512 of uid+salt): {"claimable":false,
//	  "campaigns":[{"actionType":"VIEW_DETAILS",...}]}          ← no daily credits
//	real identity (Qoder's own runtime-info helper): {"claimable":true,
//	  "campaigns":[{"actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
//	  "benefit":{"amount":100,"kind":"CREDITS",...}}]}          ← today's 100
//
// So Qoder offers the daily credits only to a device its own client
// registered. magpie-community/plugins packages/qoder 0.2.50 states the same
// rule ("without it the list answers 200 with no CLAIM_BENEFIT") and reads the
// identity from Qoder's signed helper instead of making one up:
//
//	desktop: <install>/resources/umid/runtime-info.exe
//	CLI:     <config>/.bin/umid-win32-<arch>-<hash>/runtime-info.exe
//
// SCOPE: this identity is used by the CHECK-IN path only. Chat headers keep
// the derived identity — changing them would rotate every account's chat
// fingerprint, which is a separate decision with its own upstream-risk
// question.

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// checkinIdentityTTL is how long a helper-read identity is reused. Qoder's
// desktop client renews its native identity hourly (magpie
// CAMPAIGN_MACHINE_TTL), so an identity older than that is re-read.
const checkinIdentityTTL = time.Hour

type cachedIdentity struct {
	token, typ string
	at         time.Time
}

// checkinIdentity returns the device identity the CAMPAIGNS endpoints should
// carry, in precedence order:
//
//  1. the real identity from Qoder's own runtime-info helper (cached 1h) —
//     the only source that makes the daily credits appear;
//  2. a hand-pinned pair from config, for machines with no Qoder install;
//  3. the derived identity, which upstream treats as an unknown device (the
//     old behaviour: the check-in then honestly reports "not available").
//
// The returned source is for logging/diagnostics, never for a decision.
func (d *Driver) checkinIdentity(ctx context.Context, uid string) (token, typ, source string) {
	if tok, ty, ok := d.realCheckinIdentity(ctx, uid); ok {
		return tok, ty, "runtime-info"
	}
	if tok := strings.TrimSpace(d.cfg.CheckinMachineToken); tok != "" {
		if ty := strings.TrimSpace(d.cfg.CheckinMachineType); ty != "" {
			return tok, ty, "config"
		}
	}
	t, ty := d.machineIdentityFor(uid)
	return t, ty, "derived"
}

// realCheckinIdentity reads (and caches) the identity from Qoder's helper.
// A failure is not fatal: the caller falls through to the pinned/derived
// identity, which is the pre-BP-237 behaviour.
func (d *Driver) realCheckinIdentity(ctx context.Context, uid string) (string, string, bool) {
	if uid == "" {
		return "", "", false
	}
	d.identMu.Lock()
	if c, ok := d.identCache[uid]; ok && time.Since(c.at) < checkinIdentityTTL {
		d.identMu.Unlock()
		return c.token, c.typ, true
	}
	d.identMu.Unlock()

	exe := strings.TrimSpace(d.cfg.CheckinRuntimeInfo)
	if exe == "" {
		exe = findQoderRuntimeInfo()
	}
	if exe == "" {
		return "", "", false
	}
	env := strings.TrimSpace(d.cfg.CheckinRuntimeEnv)
	if env == "" {
		env = "0" // Qoder CN
	}
	tok, ty, err := readRuntimeIdentity(ctx, exe, env, uid)
	if err != nil {
		// Logged once per read, not per request: a missing/refusing helper is
		// the expected state on a machine without Qoder installed.
		logCheckinIdentityErr(err)
		return "", "", false
	}
	d.identMu.Lock()
	if d.identCache == nil {
		d.identCache = map[string]cachedIdentity{}
	}
	d.identCache[uid] = cachedIdentity{token: tok, typ: ty, at: time.Now()}
	d.identMu.Unlock()
	return tok, ty, true
}

// readRuntimeIdentity runs the helper. The uid goes over STDIN, never on the
// command line (magpie campaignMachine does the same on Windows/macOS).
func readRuntimeIdentity(ctx context.Context, exe, environment, uid string) (string, string, error) {
	args := []string{environment, "--account-stdin"}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, args...)
	cmd.Stdin = strings.NewReader(`{"account":"` + uid + `"}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	var ident struct {
		MachineToken string `json:"machineToken"`
		MachineType  string `json:"machineType"`
	}
	if err := json.Unmarshal([]byte(line), &ident); err != nil {
		return "", "", err
	}
	if ident.MachineToken == "" || ident.MachineType == "" {
		return "", "", errNoIdentity
	}
	return ident.MachineToken, ident.MachineType, nil
}

// findQoderRuntimeInfo locates Qoder's own helper. Paths mirror magpie
// campaignRuntime: the desktop install first (user installer, then the
// machine-wide one), then the CLI's cached copy.
func findQoderRuntimeInfo() string {
	name := "runtime-info.exe"
	var dirs []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		dirs = append(dirs,
			filepath.Join(local, "Programs", "Qoder CN", "resources", "umid"),
			filepath.Join(local, "Programs", "Qoder", "resources", "umid"),
		)
	}
	for _, key := range []string{"ProgramFiles", "ProgramW6432", "PROGRAMFILES"} {
		if dir := os.Getenv(key); dir != "" {
			dirs = append(dirs,
				filepath.Join(dir, "Qoder CN", "resources", "umid"),
				filepath.Join(dir, "Qoder", "resources", "umid"),
			)
		}
	}
	// Non-default install roots (this machine keeps Qoder CN on D:).
	dirs = append(dirs,
		`D:\Program Files\Qoder CN\resources\umid`,
		`C:\Program Files\Qoder CN\resources\umid`,
	)
	for _, dir := range dirs {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	// CLI cache: <config>/.bin/umid-win32-<arch>-<hash>/runtime-info.exe
	home, _ := os.UserHomeDir()
	for _, cfgName := range []string{".qoder-cn", ".qoder"} {
		base := filepath.Join(home, cfgName, ".bin")
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "umid-win32-") {
				continue
			}
			p := filepath.Join(base, e.Name(), name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

var (
	errNoIdentity          = errString("qoder: runtime-info returned no machine identity")
	checkinIdentityErrOnce sync.Once
)

type errString string

func (e errString) Error() string { return string(e) }

// logCheckinIdentityErr prints the helper failure once per process so a
// missing helper is diagnosable without spamming every check-in.
func logCheckinIdentityErr(err error) {
	checkinIdentityErrOnce.Do(func() {
		log.Printf("qoder: check-in device identity unavailable (%v); falling back — daily credits will not be offered", err)
	})
}
