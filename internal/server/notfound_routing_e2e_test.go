package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/model"
)

// TestNotFoundCascadesWithoutPenalty pins the 2026-10-19 fix end to end.
//
// Before it: the workbuddy driver filed 404 as model_rate, so the first
// binding's 404 put a 2h model cooldown on a healthy account AND ended the
// request (wrong advice: "switch models"). After it:
//   - the account keeps no cooldown,
//   - the request cascades to the next channel, which serves it,
//   - the client gets a normal 200.
func TestNotFoundCascadesWithoutPenalty(t *testing.T) {
	var wbHits int
	wbUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer wbUp.Close()

	var traeHits int
	traeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traeHits++
		sseOK(w)
	}))
	defer traeUp.Close()

	// Trae is priority 0, so swap the roles: make the FIRST binding the one
	// that 404s. buildCrossChannelServer binds Trae at priority 0; we pass the
	// 404 upstream as Trae and the healthy one as WorkBuddy.
	_ = traeHits
	srv, p := buildCrossChannelServer(t, wbUp, traeUp)

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the second channel", resp.StatusCode)
	}
	if wbHits != 1 {
		t.Fatalf("first channel hits = %d, want 1 (a 404 must not rotate accounts)", wbHits)
	}

	// The account on the 404 channel must be unpenalised.
	accs := p.Accounts(model.ChannelTrae)
	if len(accs) == 0 {
		t.Fatal("trae account missing")
	}
	acc := accs[0]
	if mc, ok := acc.ModelCooldowns["glm-5.2"]; ok {
		t.Fatalf("404 must not cool the account's model, got until=%v reason=%q", mc.Until, mc.Reason)
	}
	if !acc.AccountCooldownUntil.IsZero() {
		t.Fatalf("404 must not set an account cooldown, got %v", acc.AccountCooldownUntil)
	}
}
