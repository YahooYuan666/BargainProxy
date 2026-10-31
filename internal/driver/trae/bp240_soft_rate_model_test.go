package trae

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-240: a 429 must scope to the MODEL, not the account.
//
// Field evidence (misakano workbuddy2api-panel PR #114, and the same defect
// fixed on workbuddy in BP-189): a soft rate limit is per model/endpoint in
// practice, but recording it account-wide makes the pool freeze every OTHER
// model the account serves for the whole rest window. On a credit pool like
// trae — which is the primary leg for glm-5.3-flash, kimi-k3.1-pro and
// deepseek — that turned one model's wall into a dead channel.
//
// The pool already honours uperr.Error.Model (pool.go falls back to an account
// rest when it is empty), so the driver only has to fill it in.
func TestSoftRateIsModelScoped(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"message":"rate limited"}`), "glm-5.3-flash")
	if e == nil {
		t.Fatal("classifyHTTP returned nil")
	}
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want %s", e.Class, uperr.ClassSoftRate)
	}
	if e.Model != "glm-5.3-flash" {
		t.Fatalf("Model = %q, want the request's model so the rest is model-scoped; "+
			"an empty Model rests the whole account", e.Model)
	}
}

// A call with no model in play (checkin, usage) must NOT invent one: the pool
// then falls back to an account rest, which is right for those paths.
func TestSoftRateWithoutModelStaysAccountScoped(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"message":"rate limited"}`), "")
	if e == nil {
		t.Fatal("classifyHTTP returned nil")
	}
	if e.Model != "" {
		t.Fatalf("Model = %q, want empty when no model is in play", e.Model)
	}
}
