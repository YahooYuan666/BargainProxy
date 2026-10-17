// 2026-10-16: the frontend auto-refreshes a stale per-process CSRF token on
// 403 and retries once. That contract depends on the 403 body naming "csrf".
package panel_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestStaleCSRFForbiddenBodyNamesCSRF(t *testing.T) {
	srv, _, _, _ := buildPanelEnv(t, nil)
	req, _ := http.NewRequest("POST", srv.URL+"/panel/api/settings",
		strings.NewReader(`{"thought_signature_replay":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "stale-token-from-before-restart")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Error, "csrf") {
		t.Fatalf("403 body %q must name csrf — the frontend keys its auto-refresh on it", body.Error)
	}
	if !strings.Contains(body.Error, "刷新") {
		t.Fatalf("403 body %q should hint at a page refresh for the un-recoverable case", body.Error)
	}
}
