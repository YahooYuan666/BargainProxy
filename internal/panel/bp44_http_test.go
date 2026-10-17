// BP-44: HTTP round trip for the experimental thought-signature replay
// setting (open-mode CSRF flow, persisted value, state echo).
package panel_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"bargainproxy/internal/panel"
)

func TestThoughtSignatureReplaySettingHTTPRoundTrip(t *testing.T) {
	srv, _, _, st := buildPanelEnv(t, nil)

	resp, err := http.Get(srv.URL + "/panel/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var st0 struct {
		CSRF     string         `json:"csrf"`
		Settings map[string]any `json:"settings"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st0)
	resp.Body.Close()
	if st0.CSRF == "" {
		t.Fatal("open mode must hand out a csrf token")
	}
	if st0.Settings["thought_signature_replay"] == true {
		t.Fatal("experimental toggle must default to off")
	}

	post := func(v bool) int {
		req, _ := http.NewRequest("POST", srv.URL+"/panel/api/settings",
			strings.NewReader(fmt.Sprintf(`{"thought_signature_replay":%v}`, v)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", st0.CSRF)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post settings: %v", err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}

	if code := post(true); code != 200 {
		t.Fatalf("enable status = %d", code)
	}
	if v, ok, _ := st.GetSetting(panel.KeyThoughtSigReplay); !ok || v != "true" {
		t.Fatalf("persisted = %q/%v, want true", v, ok)
	}
	resp2, _ := http.Get(srv.URL + "/panel/api/state")
	var st1 struct {
		Settings map[string]any `json:"settings"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&st1)
	resp2.Body.Close()
	if st1.Settings["thought_signature_replay"] != true {
		t.Fatalf("state echo = %v, want true", st1.Settings["thought_signature_replay"])
	}

	if code := post(false); code != 200 {
		t.Fatalf("disable status = %d", code)
	}
	if v, ok, _ := st.GetSetting(panel.KeyThoughtSigReplay); !ok || v != "false" {
		t.Fatalf("persisted = %q/%v, want false", v, ok)
	}
}
