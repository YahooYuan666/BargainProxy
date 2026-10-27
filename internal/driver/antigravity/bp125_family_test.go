package antigravity

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-125: Antigravity serves Gemini and 3P (Claude/GPT) from separate quota
// ledgers (AIClient2API antigravity-quota-family.js). Google's own 429 body
// ("RESOURCE_EXHAUSTED", "Quota exceeded for quota metric …") names no model,
// so the old body-contains-"model" heuristic left e.Model empty and the pool
// cooled the WHOLE account — a Gemini 429 took the account's healthy Claude
// quota down with it. Every 429 must hold the model that was asked for.
func Test429BindsTheRequestedModel(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota metric 'Generate Content API requests'."}}`)
	e := classifyHTTP(429, body, "claude-sonnet-4-5")
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate", e.Class)
	}
	if e.Model != "claude-sonnet-4-5" {
		t.Fatalf("e.Model = %q, want the requested model bound (family isolation)", e.Model)
	}
}

// A 429 with no model context at all still classifies as soft_rate; the pool
// keeps its account-level fallback for that shape.
func Test429WithoutModelStaysSoftRate(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`), "")
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate", e.Class)
	}
	if e.Model != "" {
		t.Fatalf("e.Model = %q, want empty (nothing to bind)", e.Model)
	}
}
