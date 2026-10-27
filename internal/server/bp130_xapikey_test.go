package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/config"
)

// BP-130: Anthropic SDKs authenticate every call — model lists included —
// with x-api-key, not Bearer (misakano dc75c04). Without the fallback such a
// client 401s on GET /v1/models while /v1/messages works.
func TestAuthAcceptsXAPIKey(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKey: "secret"}}
	h := s.auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	r1 := httptest.NewRequest("GET", "/v1/models", nil)
	r1.Header.Set("x-api-key", "secret")
	w1 := httptest.NewRecorder()
	h(w1, r1)
	if w1.Code != 200 {
		t.Fatalf("x-api-key auth = %d, want 200", w1.Code)
	}

	// Bearer keeps working; a wrong key still 401s.
	r2 := httptest.NewRequest("GET", "/v1/models", nil)
	r2.Header.Set("Authorization", "Bearer secret")
	w2 := httptest.NewRecorder()
	h(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("bearer auth = %d, want 200", w2.Code)
	}
	r3 := httptest.NewRequest("GET", "/v1/models", nil)
	r3.Header.Set("x-api-key", "wrong")
	w3 := httptest.NewRecorder()
	h(w3, r3)
	if w3.Code != 401 {
		t.Fatalf("wrong x-api-key = %d, want 401", w3.Code)
	}
}
