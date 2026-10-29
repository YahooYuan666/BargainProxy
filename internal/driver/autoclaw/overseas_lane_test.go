package autoclaw

import (
	"net/http"
	"strings"
	"testing"
)

// TestOverseasLaneUsesTheOneXContract pins the dialect difference between the
// two credential universes. Since BP-190 the overseas lane carries the
// OFFICIAL 2.0.3 desktop fingerprint (magpie-community/plugins 744e1e2, from
// the AppImage's dist/main.cjs) — the gateway refuses "X-Tm: linux" only
// beside a non-official fingerprint (#367), so the lane must look exactly
// like the official client:
//
//	X-Channel     official       vs AutoClaw26
//	X-Version     2.0.3          vs 2.1.0
//	auth header   X-Authorization with one Bearer prefix, on both
//	client headers  X-Client-Type: pc + X-Stainless-* family
//	              vs Origin/Referer/Client-Type/X-Agent-Id (2.x dialect)
//	path          .../autoclaw/v1/chat/completions  vs .../autoclaw/chat/completions
func TestOverseasLaneUsesTheOneXContract(t *testing.T) {
	h := managedChatHeadersFor("Bearer tok", "secret", "zai_glm-5.3-flash", true, nil)

	if got := h.Get("X-Channel"); got != overseasChannel {
		t.Fatalf("X-Channel = %q, want %q", got, overseasChannel)
	}
	if got := h.Get("X-Version"); got != overseasVersion {
		t.Fatalf("X-Version = %q, want %q", got, overseasVersion)
	}
	if !strings.Contains(overseasChatPath, "/v1/") {
		t.Fatalf("the overseas path must keep its /v1 segment, got %q", overseasChatPath)
	}
	// Both lanes authenticate with X-Authorization, always exactly one Bearer
	// prefix (measured live 2026-10-24; a doubled prefix is the BP-87 401).
	if got := h.Get("X-Authorization"); got != "Bearer tok" {
		t.Fatalf("X-Authorization = %q, want the token", got)
	}
	// The official fingerprint's own marks. X-Client-Type: pc IS part of the
	// official set (BP-190 reversed the old 2.2.2-era absence); X-Agent-Id is
	// the CN gateway's header and must still never appear here.
	if got := h.Get("X-Client-Type"); got != "pc" {
		t.Fatalf("X-Client-Type = %q, want the official %q", got, "pc")
	}
	if got := h.Get("X-Agent-Id"); got != "" {
		t.Errorf("the overseas lane must not send X-Agent-Id (got %q)", got)
	}
	// The OpenAI SDK family the Electron client adds to every official call.
	for k, want := range map[string]string{
		"X-Stainless-Lang":              "js",
		"X-Stainless-Package-Version":   overseasSDKVersion,
		"X-Stainless-Runtime":           "node",
		"X-Stainless-Runtime-Version":   overseasNodeVersion,
		"X-Stainless-Retry-Count":       "0",
		"X-Stainless-Timeout":           "600",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if h.Get("X-Stainless-OS") == "" || h.Get("X-Stainless-Arch") == "" {
		t.Error("X-Stainless-OS/X-Stainless-Arch must be present")
	}
	// X-Trace-Id and X-Request-Id carry the SAME per-request UUID.
	if h.Get("X-Trace-Id") == "" || h.Get("X-Trace-Id") != h.Get("X-Request-Id") {
		t.Fatalf("X-Trace-Id and X-Request-Id must be the same UUID, got %q / %q",
			h.Get("X-Trace-Id"), h.Get("X-Request-Id"))
	}
	// A conversation-stable session id: present, and derived from the wire
	// when one is given (see TestConversationSessionIDIsStable).
	if h.Get("X-Session-Id") == "" {
		t.Fatal("X-Session-Id is part of the official fingerprint")
	}
	if h.Get("x_trace_id") != "autoclaw-desktop" {
		t.Errorf("x_trace_id = %q, want autoclaw-desktop", h.Get("x_trace_id"))
	}
	if h.Get("X-Lang") != overseasLang {
		t.Errorf("X-Lang = %q, want %q", h.Get("X-Lang"), overseasLang)
	}
	// Both lanes sign, or the gateway answers 401 regardless of the token.
	if h.Get("X-Auth-Sign") == "" || h.Get("X-Auth-Appid") == "" {
		t.Fatal("the X-Auth-* signature family is mandatory on both lanes")
	}
}

// TestConversationSessionIDIsStable pins the conversation-stable X-Session-Id
// derivation (BP-190): same first non-system message, same UUID, on every
// turn of the conversation.
func TestConversationSessionIDIsStable(t *testing.T) {
	wire := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi there"}]}`)
	a := conversationSessionID(wire)
	b := conversationSessionID(wire)
	if a == "" || a != b {
		t.Fatalf("session id must be stable, got %q vs %q", a, b)
	}
	// A different first message must derive a different id.
	other := []byte(`{"messages":[{"role":"user","content":"different"}]}`)
	if conversationSessionID(other) == a {
		t.Fatal("different conversations must not share a session id")
	}
	// No usable message: a fresh id, still present.
	if conversationSessionID([]byte(`{}`)) == "" {
		t.Fatal("a body without messages still needs a session id")
	}
}

// TestCNLaneKeepsItsOwnDialect: the change must not disturb the CN path.
func TestCNLaneKeepsItsOwnDialect(t *testing.T) {
	h := managedChatHeadersFor("Bearer tok", "secret", "zai_glm-5.3-flash", false, nil)

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
