// BP-44: panel-side wiring of the experimental thought-signature replay
// toggle — persisted setting, state echo, restore, and the settings-page
// checkbox.
package panel

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// stubSigDriver mirrors the driver capability without any upstream wiring.
type stubSigDriver struct {
	driver.Driver
	enabled bool
}

func (s *stubSigDriver) SetThoughtSignatureReplay(v bool) { s.enabled = v }

func TestRestoreHotSettingsAppliesThoughtSignatureReplay(t *testing.T) {
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	st := store.NewMemory()
	if err := st.SetSetting(KeyThoughtSigReplay, "true"); err != nil {
		t.Fatalf("set: %v", err)
	}
	stub := &stubSigDriver{}
	gw := gateway.New(cfg, pool.New(pool.Params{}, map[string]int{model.ChannelAntigravity: 1}, nil),
		router.New(cfg), map[string]driver.Driver{model.ChannelAntigravity: stub}, st)

	applied := RestoreHotSettings(gw, st)
	if !stub.enabled {
		t.Fatal("RestoreHotSettings did not enable the driver toggle from the persisted setting")
	}
	found := false
	for _, a := range applied {
		if a == "thought_signature_replay=true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("startup log entries missing the applied setting: %v", applied)
	}
}

func TestPanelShipsThoughtSignatureCheckbox(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		`id="setThoughtSigReplay"`,
		"试验功能（默认关）",
		"hs.thought_signature_replay === true",
		"body.thought_signature_replay",
		"thought_signature",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
}

// The hot-settings view echoes the toggle (UI restore path).
func TestHotSettingsViewEchoesToggle(t *testing.T) {
	st := store.NewMemory()
	if err := st.SetSetting(KeyThoughtSigReplay, "true"); err != nil {
		t.Fatalf("set: %v", err)
	}
	p := &Panel{store: st}
	view := p.hotSettingsView()
	if view["thought_signature_replay"] != true {
		t.Fatalf("hotSettingsView = %v, want thought_signature_replay=true", view["thought_signature_replay"])
	}
	// Default (absent) must read as false — the feature is opt-in.
	st2 := store.NewMemory()
	p2 := &Panel{store: st2}
	if p2.hotSettingsView()["thought_signature_replay"] == true {
		t.Fatal("absent setting must default to false")
	}
	// JSON round-trip sanity for the settings payload key.
	b, _ := json.Marshal(map[string]any{"thought_signature_replay": true})
	if string(b) != `{"thought_signature_replay":true}` {
		t.Fatalf("payload key changed: %s", b)
	}
}
