package panel_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postWbRegister calls the WorkBuddy registration endpoint.
func postWbRegister(t *testing.T, srvURL, csrf, target string, dryRun bool) map[string]any {
	t.Helper()
	body := map[string]any{"target": target, "dry_run": dryRun}
	resp := postJSON(t, srvURL, "/panel/api/workbuddy/register", csrf, body)
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %v", resp.StatusCode, out["error"])
	}
	return out
}

func TestWorkBuddyRegisterDryRunAndWrite(t *testing.T) {
	srv, _ := buildPanelServer(t)
	csrf := getCSRF(t, srv.URL)
	target := filepath.Join(t.TempDir(), "models.json")

	// Dry run: reports the plan, writes nothing.
	dry := postWbRegister(t, srv.URL, csrf, target, true)
	if dry["wrote"].(bool) {
		t.Fatal("dry run must not write")
	}
	if n, ok := dry["generated"].(float64); !ok || n < 1 {
		t.Fatalf("dry run generated = %v, want >= 1 (config seeds one routed model)", dry["generated"])
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run created the file: %v", err)
	}

	// Real write: file appears with the gateway model, chat protocol.
	got := postWbRegister(t, srv.URL, csrf, target, false)
	if got["wrote"] != true {
		t.Fatalf("write result wrote = %v", got["wrote"])
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatalf("models.json parse: %v", err)
	}
	if len(entries) < 1 {
		t.Fatal("no entries written")
	}
	first := entries[0]
	for _, key := range []string{"id", "url", "apiKey", "useCustomProtocol", "maxInputTokens"} {
		if _, ok := first[key]; !ok {
			t.Errorf("entry missing %q: %v", key, first)
		}
	}
	if first["vendor"] != "BargainProxy" {
		t.Errorf("vendor = %v", first["vendor"])
	}
	if first["useCustomProtocol"] != false {
		t.Errorf("useCustomProtocol = %v, want false (OpenAI chat)", first["useCustomProtocol"])
	}
	if !strings.Contains(first["url"].(string), "/v1") {
		t.Errorf("url = %v, want …/v1", first["url"])
	}
}

func TestWorkBuddyRegisterPreservesUserEntries(t *testing.T) {
	srv, _ := buildPanelServer(t)
	csrf := getCSRF(t, srv.URL)
	target := filepath.Join(t.TempDir(), "models.json")

	// First write creates BargainProxy entries.
	postWbRegister(t, srv.URL, csrf, target, false)

	// The user then adds their own model (different vendor).
	existing, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	_ = json.Unmarshal(existing, &entries)
	entries = append(entries, map[string]any{
		"id": "my-own-model", "vendor": "Custom", "url": "https://example.com/v1", "apiKey": "***",
	})
	// And a user-owned entry that shares an id with a gateway model: it wins.
	bp := entries[0]
	entries = append(entries, map[string]any{
		"id": bp["id"], "vendor": "Custom", "url": "https://mine.example/v1", "apiKey": "***",
	})
	b2, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(target, b2, 0o644); err != nil {
		t.Fatal(err)
	}

	got := postWbRegister(t, srv.URL, csrf, target, false)
	if got["skipped_user"].(float64) != 1 {
		t.Errorf("skipped_user = %v, want 1", got["skipped_user"])
	}
	b3, _ := os.ReadFile(target)
	var after []map[string]any
	_ = json.Unmarshal(b3, &after)
	byID := map[string]map[string]any{}
	for _, e := range after {
		byID[e["id"].(string)] = e
	}
	if byID["my-own-model"] == nil {
		t.Error("user-only entry lost")
	}
	if v, ok := byID[bp["id"].(string)]; !ok || v["url"] != "https://mine.example/v1" {
		t.Errorf("user-owned same-id entry overwritten: %v", v)
	}
}

func TestWorkBuddyRegisterPrunesStale(t *testing.T) {
	srv, _ := buildPanelServer(t)
	csrf := getCSRF(t, srv.URL)
	target := filepath.Join(t.TempDir(), "models.json")

	// A stale BargainProxy entry that is no longer routed gets pruned;
	// a stale user entry stays.
	stale := []map[string]any{
		{"id": "ghost-model", "vendor": "BargainProxy", "url": "http://127.0.0.1:1/v1", "apiKey": "x"},
		{"id": "user-ghost", "vendor": "Custom", "url": "http://127.0.0.1:1/v1", "apiKey": "x"},
	}
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := postWbRegister(t, srv.URL, csrf, target, false)
	if got["removed"].(float64) != 1 {
		t.Errorf("removed = %v, want 1 (only the BargainProxy ghost)", got["removed"])
	}
	b2, _ := os.ReadFile(target)
	var after []map[string]any
	_ = json.Unmarshal(b2, &after)
	ids := map[string]bool{}
	for _, e := range after {
		ids[e["id"].(string)] = true
	}
	if ids["ghost-model"] {
		t.Error("stale BargainProxy entry survived")
	}
	if !ids["user-ghost"] {
		t.Error("user entry wrongly pruned")
	}
}

func TestWorkBuddyRegisterGetIsHarmless(t *testing.T) {
	srv, _ := buildPanelServer(t)
	// The GET falls through to the panel index (mux prefix route); the point
	// is that no registration happens without a POST.
	resp, err := http.Get(srv.URL + "/panel/api/workbuddy/register")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Fatalf("GET status = %d, want non-5xx", resp.StatusCode)
	}
}
