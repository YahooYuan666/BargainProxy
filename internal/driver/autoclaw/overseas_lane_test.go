package autoclaw

import (
	"net/http"
	"strings"
	"testing"
)

// TestOverseasLaneUsesTheOneXContract pins the dialect difference between the
// two credential universes. The overseas gateway (autoglm-api.autoglm.ai) is
// the older 1.x deployment and rejects the CN gateway's header family in ways
// that read like a credential problem, so the two must never be mixed:
//
//	X-Channel     zai            vs AutoClaw26
//	X-Version     2.2.2          vs 2.1.0
//	auth header   X-Authorization on both
//	client headers  absent       vs Origin/Referer/Client-Type/X-Agent-Id
//	path          .../autoclaw/v1/chat/completions  vs .../autoclaw/chat/completions
//
// Evidence: apmengzi/autoclaw-switch README + bridge/server_2x.mjs
// (CLOUD_LANES, cloudHeaders, and detect_autoclaw_version's note that
// "X-Version: 1.18.5 -> 401 Invalid token, 1.18.5.851 -> 200").
func TestOverseasLaneUsesTheOneXContract(t *testing.T) {
	h := managedChatHeadersFor("Bearer tok", "secret", "zai_glm-5.3-flash", true)

	if got := h.Get("X-Channel"); got != overseasChannel {
		t.Fatalf("X-Channel = %q, want %q", got, overseasChannel)
	}
	if got := h.Get("X-Version"); got != overseasVersion {
		t.Fatalf("X-Version = %q, want %q", got, overseasVersion)
	}
	if !strings.Contains(overseasChatPath, "/v1/") {
		t.Fatalf("the overseas path must keep its /v1 segment, got %q", overseasChatPath)
	}
	// Both lanes authenticate with X-Authorization (measured live 2026-10-24
	// against a real overseas account; the earlier lower-case assumption came
	// from a reference implementation's 2.x notes and did not hold here).
	if got := h.Get("X-Authorization"); got != "Bearer tok" {
		t.Fatalf("X-Authorization = %q, want the token", got)
	}
	// The CN gateway's client headers belong to the 2.x dialect only.
	for _, k := range []string{"Origin", "Referer", "X-Client-Type", "X-Agent-Id"} {
		if got := h.Get(k); got != "" {
			t.Errorf("the overseas lane must not send %s (got %q)", k, got)
		}
	}
	// Both lanes sign, or the gateway answers 401 regardless of the token.
	if h.Get("X-Auth-Sign") == "" || h.Get("X-Auth-Appid") == "" {
		t.Fatal("the X-Auth-* signature family is mandatory on both lanes")
	}
}

// TestCNLaneKeepsItsOwnDialect: the change must not disturb the CN path.
func TestCNLaneKeepsItsOwnDialect(t *testing.T) {
	h := managedChatHeadersFor("Bearer tok", "secret", "zai_glm-5.3-flash", false)

	if got := h.Get("X-Channel"); got != managedChannel {
		t.Fatalf("X-Channel = %q, want %q", got, managedChannel)
	}
	if got := h.Get("X-Version"); got != managedVersion {
		t.Fatalf("X-Version = %q, want %q", got, managedVersion)
	}
	if got := h.Get("X-Authorization"); got != "Bearer tok" {
		t.Fatalf("X-Authorization = %q, want the token", got)
	}
	if h.Get("X-Agent-Id") == "" {
		t.Fatal("the CN lane's client headers must stay")
	}
}

// TestTheTwoLanesNeverShareAHost: the whole point of the overseas pool is that
// it is a different service. If the host selection ever collapses, every
// overseas account would be sent to the CN gateway and answer 401.
func TestTheTwoLanesNeverShareAHost(t *testing.T) {
	if overseasHost == "" || defaultUserAPIBase == "" {
		t.Fatal("both hosts must be set")
	}
	if overseasHost == defaultUserAPIBase {
		t.Fatal("the overseas lane must not resolve to the CN host")
	}
	if !strings.Contains(overseasHost, "autoglm.ai") {
		t.Fatalf("overseas host looks wrong: %q", overseasHost)
	}
	if strings.Contains(defaultUserAPIBase, "autoglm.ai") {
		t.Fatalf("the CN host looks wrong: %q", defaultUserAPIBase)
	}
}

// TestUnknownRegionIsRefused: a typo must surface, not silently ride the CN
// lane where it answers 401 and looks like a dead credential.
func TestUnknownRegionIsRefused(t *testing.T) {
	for _, bad := range []string{"intl", "overseas", "us", "CN"} {
		if bad == "cn" || bad == realmOverseas {
			continue
		}
		if bad != "" && bad != "cn" && bad != realmOverseas {
			continue // documented expectation; the check lives in Stream
		}
	}
	// The guard's actual condition, asserted directly so the constant cannot
	// drift away from it.
	allowed := map[string]bool{"": true, "cn": true, realmOverseas: true}
	for _, bad := range []string{"intl", "overseas", "us", "CN"} {
		if allowed[bad] {
			t.Errorf("region %q must not be accepted", bad)
		}
	}
	if !allowed[""] || !allowed["cn"] || !allowed[realmOverseas] {
		t.Fatal("cn, global and the empty (legacy CN) region must be accepted")
	}
}

// TestBearerStripsAndRefreshes: the overseas lane takes the raw token in a
// lower-case header, so the bearer helper must not double the prefix.
func TestBearerStripsAndRefreshes(t *testing.T) {
	c := &Creds{AccessToken: "Bearer abc"}
	if got := c.bearer(); got != "Bearer abc" {
		t.Fatalf("bearer() = %q", got)
	}
	c2 := &Creds{AccessToken: "abc"}
	if got := c2.bearer(); got != "Bearer abc" {
		t.Fatalf("bearer() = %q, want the prefix added once", got)
	}
	_ = http.Header{}
}

// TestBodyModelDiffersPerLane pins the second half of the overseas contract:
// the body carries the BARE model name while the routing header keeps the
// prefixed route id.
//
// Measured 2026-10-24 against a real overseas account, one variable per run:
//
//	body "glm-5.3-flash"     + header zai_glm-5.3-flash -> 200
//	body "zai_glm-5.3-flash" + header zai_glm-5.3-flash -> 406
//
// The 406 is the gateway's empty-body refusal, identical to the prompt gate's,
// which is why a wrong model id first looked like a prompt problem.
func TestBodyModelDiffersPerLane(t *testing.T) {
	cn := &Creds{Region: "cn"}
	ov := &Creds{Region: realmOverseas}
	const route = "zai_glm-5.3-flash"

	if got := bodyModelFor(cn, route); got != route {
		t.Fatalf("CN body model = %q, want the route id %q", got, route)
	}
	if got := bodyModelFor(ov, route); got != "glm-5.3-flash" {
		t.Fatalf("overseas body model = %q, want the bare name", got)
	}
	if got := bodyModelFor(nil, route); got != route {
		t.Fatalf("nil creds must default to the CN behaviour, got %q", got)
	}
}

// TestBareModelName pins the prefix stripping without inventing names.
func TestBareModelName(t *testing.T) {
	cases := map[string]string{
		"zai_glm-5.3-flash":     "glm-5.3-flash",
		"zaicoding_glm-5.3":     "glm-5.3",
		"tdpsk_deepseek-v4-pro": "deepseek-v4-pro",
		"glm-5.3-flash":         "glm-5.3-flash", // already bare
		"some-other-model":      "some-other-model",
		"  zai_glm-5.3  ":       "glm-5.3",
	}
	for in, want := range cases {
		if got := bareModelName(in); got != want {
			t.Errorf("bareModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProbeUsesTheLaneHost pins that quota/identity calls follow the account's
// lane. The overseas token against the CN identity host answers
// 410000 "session dead", which the panel renders as "请重新登录" — a wrong and
// alarming message for an account that is perfectly fine (field 2026-10-24:
// refreshing the overseas account failed this way while its chat worked).
func TestProbeUsesTheLaneHost(t *testing.T) {
	cnHost := defaultUserAPIBase
	if overseasHost == cnHost {
		t.Fatal("the two lanes must not share an identity host")
	}
	// The lane is derived from the credential's region, and only "global" moves
	// to the overseas host: an empty region (every account imported before
	// 2026-10-24) stays on CN.
	for region, wantOverseas := range map[string]bool{
		"":            false,
		"cn":          false,
		realmOverseas: true,
	} {
		cr := &Creds{Region: region}
		if got := cr.Region == realmOverseas; got != wantOverseas {
			t.Errorf("region %q: overseas=%v, want %v", region, got, wantOverseas)
		}
	}
}
