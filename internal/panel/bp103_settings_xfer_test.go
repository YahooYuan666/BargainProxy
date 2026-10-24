package panel_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"bargainproxy/internal/model"
)

// getCSRF fetches the open-mode CSRF token (same dance the panel JS does).
func getCSRF(t *testing.T, srvURL string) string {
	t.Helper()
	resp, err := http.Get(srvURL + "/panel/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	var st struct {
		CSRF string `json:"csrf"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.CSRF == "" {
		t.Fatal("state returned no csrf token")
	}
	return st.CSRF
}

func postJSON(t *testing.T, srvURL, path, csrf string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srvURL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// seedSetting writes one settings key through the panel's settings endpoint
// (same path the settings page uses, so no store handle is needed).
func seedSetting(t *testing.T, srvURL, csrf, key, value string) {
	t.Helper()
	body := map[string]any{}
	switch key {
	case "greedy_floor_seconds":
		if value == "" {
			return // no-op: the endpoint only writes when the field is present
		}
		body["greedy_floor_seconds"] = mustInt(t, value)
	default:
		t.Fatalf("seedSetting: unsupported key %q", key)
	}
	resp := postJSON(t, srvURL, "/panel/api/settings", csrf, body)
	resp.Body.Close()
}

func mustInt(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("mustInt: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

const xferRoutes = `[{"model":"glm-5.3","channel":"workbuddy","upstream_model":"glm-5.3","priority":0},{"model":"glm-5.3","channel":"trae","upstream_model":"glm-5.3-flashx","priority":4}]`

// bundle builds a valid transfer payload.
func bundle() map[string]any {
	return map[string]any{
		"version": 1,
		"keys": map[string]string{
			"model_routes":              xferRoutes,
			"greedy_floor_seconds":      "600",
			"max_in_flight":             `{"workbuddy":3}`,
			"workbuddy_global":          "true",
			"prompt_replace_enabled":    "true",
			"include_disabled_in_tasks": "false",
		},
		"task_params": map[string]any{
			"checkin_hours": []int{9, 21}, "growth_hours": []int{9},
			"activity_hours": []int{10}, "travel_hours": []int{9, 21},
			"blackcat_hours": []int{23},
			"probe_minutes":  5, "google_probe_minutes": 15, "refresh_minutes": 60,
		},
		"primary_models": []string{"glm-5.3"},
	}
}

func TestSettingsExportImportRoundTrip(t *testing.T) {
	srv, _ := buildPanelServer(t)
	csrf := getCSRF(t, srv.URL)

	// Seed the source machine's tuned state: a scheduling knob and a route
	// row beyond config's default (through the panel's own PUT endpoint, so
	// the hot route table and the settings blob both move).
	seedSetting(t, srv.URL, csrf, "greedy_floor_seconds", "300")
	putBody := map[string]any{"routes": []map[string]any{
		{"model": "kimi-k3", "channel": "workbuddy", "upstream_model": "kimi-k3", "priority": 1},
	}}
	b, _ := json.Marshal(putBody)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/panel/api/routes", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp0, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seed routes: %v", err)
	}
	resp0.Body.Close()

	// Export.
	resp, err := http.Get(srv.URL + "/panel/api/settings/export")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	var bundle struct {
		Version       int               `json:"version"`
		Keys          map[string]string `json:"keys"`
		PrimaryModels []string          `json:"primary_models"`
		TaskParams    *struct {
			CheckinHours []int `json:"checkin_hours"`
		} `json:"task_params"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	resp.Body.Close()
	if bundle.Version != 1 {
		t.Fatalf("version = %d, want 1", bundle.Version)
	}
	if bundle.Keys["greedy_floor_seconds"] != "300" {
		t.Errorf("export missing greedy_floor_seconds=300, got %q", bundle.Keys["greedy_floor_seconds"])
	}
	if bundle.Keys["model_routes"] == "" || !strings.Contains(bundle.Keys["model_routes"], "kimi-k3") {
		t.Errorf("export missing model_routes with seeded row, got %q", bundle.Keys["model_routes"])
	}
	for _, forbidden := range []string{"panel_password_hash", "login-flow:any", "task_last_fired_growth"} {
		if _, ok := bundle.Keys[forbidden]; ok {
			t.Errorf("export must not carry %s", forbidden)
		}
	}
	if len(bundle.PrimaryModels) != 0 {
		t.Errorf("export PrimaryModels = %v, want empty (cfg default)", bundle.PrimaryModels)
	}

	// Wipe the local values, then import the same bundle back and verify
	// through the export endpoint (no store handle needed in http-land).
	// The whitelist is sent explicitly non-empty so the applied list shows it.
	seedSetting(t, srv.URL, csrf, "greedy_floor_seconds", "")
	imp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, map[string]any{
		"version":        1,
		"keys":           bundle.Keys,
		"primary_models": []string{"glm-5.3"},
	})
	if imp.StatusCode != 200 {
		var e map[string]any
		_ = json.NewDecoder(imp.Body).Decode(&e)
		imp.Body.Close()
		t.Fatalf("import status = %d: %v", imp.StatusCode, e["error"])
	}
	var out struct {
		Applied []string `json:"applied"`
	}
	_ = json.NewDecoder(imp.Body).Decode(&out)
	imp.Body.Close()
	exp2, err := http.Get(srv.URL + "/panel/api/settings/export")
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	var again struct {
		Keys map[string]string `json:"keys"`
	}
	_ = json.NewDecoder(exp2.Body).Decode(&again)
	exp2.Body.Close()
	if again.Keys["greedy_floor_seconds"] != "300" {
		t.Errorf("after import greedy_floor_seconds = %q, want 300", again.Keys["greedy_floor_seconds"])
	}
	if !strings.Contains(again.Keys["model_routes"], "kimi-k3") {
		t.Errorf("after import model_routes missing kimi-k3, got %q", again.Keys["model_routes"])
	}
	joined := strings.Join(out.Applied, ",")
	if !strings.Contains(joined, "primary_models") {
		t.Errorf("applied = %v, want primary_models present", out.Applied)
	}
}

func TestSettingsImportRejectsForeignKeys(t *testing.T) {
	srv, _, _, st := buildPanelEnv(t, nil)
	csrf := getCSRF(t, srv.URL)

	b := bundle()
	b["keys"].(map[string]string)["panel_password_hash"] = "$2a$10$crafted"
	resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var e map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if !strings.Contains(e["error"].(string), "panel_password_hash") {
		t.Errorf("error = %v, want it to name the offending key", e["error"])
	}
	// Nothing was written.
	if _, ok, _ := st.GetSetting("greedy_floor_seconds"); ok {
		t.Error("rejected import must not write any key")
	}

	// A login-flow key is equally rejected.
	b = bundle()
	b["keys"].(map[string]string)["login-flow:z:abc"] = "sealed"
	resp = postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b)
	if resp.StatusCode != 400 {
		t.Fatalf("login-flow status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsImportRejectsBadVersionAndRoutes(t *testing.T) {
	srv, _, _, _ := buildPanelEnv(t, nil)
	csrf := getCSRF(t, srv.URL)

	b := bundle()
	b["version"] = 99
	if resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b); resp.StatusCode != 400 {
		t.Fatalf("version status = %d, want 400", resp.StatusCode)
	}

	b = bundle()
	b["version"] = 1
	b["keys"].(map[string]string)["model_routes"] = `[{"model":"","channel":"workbuddy","upstream_model":"x","priority":0}]`
	resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b)
	if resp.StatusCode != 400 {
		t.Fatalf("empty-model route status = %d, want 400", resp.StatusCode)
	}
	var e map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if !strings.Contains(e["error"].(string), "model_routes") {
		t.Errorf("error = %v, want model_routes named", e["error"])
	}

	b["keys"].(map[string]string)["model_routes"] = "not json"
	if resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b); resp.StatusCode != 400 {
		t.Fatalf("bad json routes status = %d, want 400", resp.StatusCode)
	}
}

func TestSettingsImportAppliesTaskParamsAndRejectsBadHours(t *testing.T) {
	srv, pn, _, _ := buildPanelEnv(t, nil)
	csrf := getCSRF(t, srv.URL)

	// Install a task runner so the task_params branch has a provider; keep
	// the reference because the fake persists in memory, not in the store.
	fake := &fakeTaskParams{fakeTaskRunner: fakeTaskRunner{enabled: map[string]bool{}}, cur: model.TaskParams{
		CheckinHours: []int{9}, GrowthHours: []int{9}, ActivityHours: []int{10},
		TravelHours: []int{9}, BlackcatHours: []int{23},
		ProbeMinutes: 5, GoogleProbeMinutes: 15, RefreshMinutes: 60,
	}}
	pn.SetTaskRunner(fake)

	b := bundle()
	resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b)
	if resp.StatusCode != 200 {
		var e map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		t.Fatalf("status = %d: %v", resp.StatusCode, e["error"])
	}
	resp.Body.Close()
	if len(fake.cur.CheckinHours) != 2 || fake.cur.CheckinHours[0] != 9 || fake.cur.CheckinHours[1] != 21 {
		t.Errorf("after import CheckinHours = %v, want [9 21]", fake.cur.CheckinHours)
	}

	b["task_params"].(map[string]any)["checkin_hours"] = []int{25}
	if resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b); resp.StatusCode != 400 {
		t.Fatalf("hour 25 status = %d, want 400", resp.StatusCode)
	}

	b["task_params"].(map[string]any)["checkin_hours"] = []int{9, 21}
	b["task_params"].(map[string]any)["probe_minutes"] = 0
	if resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b); resp.StatusCode != 400 {
		t.Fatalf("probe_minutes 0 status = %d, want 400", resp.StatusCode)
	}
}

func TestSettingsImportEmptyWhitelistClears(t *testing.T) {
	srv, _, _, _ := buildPanelEnv(t, nil)
	csrf := getCSRF(t, srv.URL)
	b := bundle()
	b["primary_models"] = []string{}
	resp := postJSON(t, srv.URL, "/panel/api/settings/import", csrf, b)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}
