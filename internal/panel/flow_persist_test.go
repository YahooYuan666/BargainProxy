// BP-51: poll-based onboarding flows (ZCode CLI / WorkBuddy device) survive a
// gateway restart — the user-reported "登录会话不存在或已过期" happened because
// a delivery restart wiped the in-memory flow map. Loopback-callback flows
// (Google / Trae) cannot survive (their listener is gone) and stay in memory.
package panel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

var testFlowMasterKey = []byte("0123456789abcdef0123456789abcdef")

func TestPollFlowSurvivesRestart(t *testing.T) {
	st := store.NewMemory()
	cfg := config.Default()
	p := New(cfg, nil, st, testFlowMasterKey)

	exp := time.Now().Add(5 * time.Minute)
	p.putFlow("z:flow-1", &authFlow{channel: model.ChannelZCode, state: "poll-token", status: "pending", expiresAt: exp, created: time.Now()})
	p.putFlow("g:nonce", &authFlow{channel: model.ChannelAntigravity, status: "pending", expiresAt: exp, created: time.Now()})

	p2 := New(cfg, nil, st, testFlowMasterKey) // simulates the restart
	f, ok := p2.getFlow("z:flow-1")
	if !ok || f.state != "poll-token" || f.channel != model.ChannelZCode || f.status != "pending" {
		t.Fatalf("zcode flow lost across restart: %+v ok=%v", f, ok)
	}
	if !f.expiresAt.After(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("expiry not restored: %v", f.expiresAt)
	}
	if _, ok := p2.getFlow("g:nonce"); ok {
		t.Fatal("loopback flow must not survive a restart (its listener is gone)")
	}
	// The poll token is a bearer: the blob must be sealed, not readable.
	blob, _, _ := st.GetSetting(flowSettingPrefix + "z:flow-1")
	if blob == "" || strings.Contains(blob, "poll-token") {
		t.Fatalf("flow blob must be sealed: %q", blob)
	}
}

func TestPollFlowExpiryIsDroppedOnLoad(t *testing.T) {
	st := store.NewMemory()
	cfg := config.Default()
	p := New(cfg, nil, st, testFlowMasterKey)
	p.putFlow("w:stale", &authFlow{
		channel: model.ChannelWorkBuddy, status: "pending",
		expiresAt: time.Now().Add(-2 * flowPersistEvery), created: time.Now().Add(-time.Hour),
	})

	p2 := New(cfg, nil, st, testFlowMasterKey)
	if _, ok := p2.getFlow("w:stale"); ok {
		t.Fatal("long-expired flow must not load")
	}
	if blob, _, _ := st.GetSetting(flowSettingPrefix + "w:stale"); blob != "" {
		t.Fatalf("expired flow blob must be dropped on load, got %q", blob)
	}
}

// Terminal states are persisted too: a poll after a restart still answers with
// the outcome instead of "会话不存在".
func TestTerminalFlowStateSurvivesRestart(t *testing.T) {
	st := store.NewMemory()
	cfg := config.Default()
	p := New(cfg, nil, st, testFlowMasterKey)
	p.putFlow("z:flow-9", &authFlow{channel: model.ChannelZCode, state: "t", status: "pending", expiresAt: time.Now().Add(5 * time.Minute), created: time.Now()})
	p.finishFlow("z:flow-9", "success", "zcode:u-1", "")

	p2 := New(cfg, nil, st, testFlowMasterKey)
	f, ok := p2.getFlow("z:flow-9")
	if !ok || f.status != "success" || f.accountID != "zcode:u-1" {
		t.Fatalf("terminal state lost: %+v ok=%v", f, ok)
	}
}

// An actively polled flow slides (the user takes their time authorizing) but
// never past the hard cap, and the slide is persisted once the throttle window
// has passed.
func TestTouchFlowSlidesWithHardCapAndPersists(t *testing.T) {
	st := store.NewMemory()
	cfg := config.Default()
	p := New(cfg, nil, st, testFlowMasterKey)

	created := time.Now().Add(-4 * time.Minute) // 4 minutes into a 5-min TTL
	p.putFlow("z:flow-2", &authFlow{
		channel: model.ChannelZCode, state: "t", status: "pending",
		expiresAt: created.Add(oauthFlowTTL), created: created,
	})
	p.touchFlow("z:flow-2")
	f, ok := p.getFlow("z:flow-2")
	if !ok {
		t.Fatal("slid flow must stay live")
	}
	if f.expiresAt.Sub(time.Now()) < 4*time.Minute {
		t.Fatalf("expiry not slid: %v", f.expiresAt)
	}

	// The same slide against a 28-minute-old flow is clamped to the cap.
	old := time.Now().Add(-flowMaxLifetime + 2*time.Minute)
	p.putFlow("w:old", &authFlow{channel: model.ChannelWorkBuddy, status: "pending",
		expiresAt: old.Add(oauthFlowTTL), created: old})
	p.touchFlow("w:old")
	fo, ok := p.getFlow("w:old")
	if !ok {
		t.Fatal("capped flow must stay live")
	}
	want := old.Add(flowMaxLifetime)
	if fo.expiresAt.After(want.Add(time.Second)) {
		t.Fatalf("slide must clamp at created+cap: got %v want ≤%v", fo.expiresAt, want)
	}

	// With the throttle window elapsed, a further slide is written through.
	p.flowMu.Lock()
	p.flows["z:flow-2"].expiresAt = time.Now().Add(time.Minute) // as if some time passed
	p.flows["z:flow-2"].persistedAt = time.Now().Add(-2 * flowPersistEvery)
	p.flowMu.Unlock()
	p.touchFlow("z:flow-2")
	blob, _, _ := st.GetSetting(flowSettingPrefix + "z:flow-2")
	plain, err := p.box.Open(blob)
	if err != nil {
		t.Fatalf("open slid blob: %v", err)
	}
	var pf persistedFlow
	if err := json.Unmarshal(plain, &pf); err != nil {
		t.Fatalf("decode slid blob: %v", err)
	}
	if pf.ExpiresAt.Sub(time.Now()) < 4*time.Minute {
		t.Fatalf("slid expiry not persisted: %s", plain)
	}
}
