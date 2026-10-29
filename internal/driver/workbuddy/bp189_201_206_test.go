package workbuddy

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-189 (misakano 22868bbf, PR #114): a plain soft rate limit binds to the
// MODEL, not the account — one model's wall must not cool the other models
// the same account could still serve. The billing path (no model) keeps the
// account-level fallback.
func TestBP189SoftRateCarriesTheModel(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"error":"rate"}`), "deepseek-v4.1-flash")
	if e.Model != "deepseek-v4.1-flash" {
		t.Fatalf("the 429 must carry the model, got %q", e.Model)
	}
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s", e.Class)
	}
	e2 := classifyHTTP(429, []byte(`{"error":"rate"}`), "")
	if e2.Model != "" {
		t.Fatalf("a model-less context must stay account-level, got %q", e2.Model)
	}
}

// BP-201 (plugins 3f73907): the meter checkin's own refusal family — 1001
// already checked in, 1002 not eligible, 1003 not open — is the day's done
// state, never a retryable failure.
func TestBP201CheckinCodeFamily(t *testing.T) {
	for _, code := range []int{1001, 1002, 1003} {
		e := classifyBusiness(code, "denied", "")
		if !IsAlreadyCheckin(e) {
			t.Fatalf("code %d must read as the day's done state, got %v", code, e)
		}
	}
	// The pre-existing 10001 answer and the wording stay recognized.
	if !IsAlreadyCheckin(classifyBusiness(10001, "已经签到过了", "")) {
		t.Fatal("10001 must still read as already checked in")
	}
	// The transient marker parses from both spellings.
	if checkinBodyCode([]byte(`{"code":10000}`)) != 10000 {
		t.Fatal("numeric code 10000 must parse")
	}
	if checkinBodyCode([]byte(`{"code":"1001"}`)) != 1001 {
		t.Fatal("string codes must parse")
	}
	if checkinBodyCode([]byte(`{}`)) != 0 {
		t.Fatal("no code is no code")
	}
}

// BP-206 (Coding2API bd48203): the official client sends X-User-Id on every
// request; a token-only import derives the same identity from the JWT "sub".
func TestBP206UIDFromJWT(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"sub": "u-123", "exp": 999})
	tok := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	if got := uidFromJWT(tok); got != "u-123" {
		t.Fatalf("uid = %q, want u-123", got)
	}
	if got := uidFromJWT("not-a-jwt"); got != "" {
		t.Fatalf("a non-JWT must yield nothing, got %q", got)
	}
	payload2, _ := json.Marshal(map[string]any{"exp": 999})
	tok2 := "h." + base64.RawURLEncoding.EncodeToString(payload2) + ".s"
	if got := uidFromJWT(tok2); got != "" {
		t.Fatalf("a JWT without sub must yield nothing, got %q", got)
	}
}
