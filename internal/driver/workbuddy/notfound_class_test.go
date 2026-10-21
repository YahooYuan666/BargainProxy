package workbuddy

import (
	"net/http"
	"testing"

	"bargainproxy/internal/uperr"
)

// TestNotFoundIsNotModelRate is the regression this file exists for
// (2026-10-19, misakano7545 comparison): a 404 used to classify as
// ClassModelRate, which cools the account for that model for up to
// ModelRateHoldCap (2h). A 404 means "this path/model does not exist", not
// "this account is rate limited" — penalizing the account hides a channel-wide
// routing break as a per-account model problem, and a batch of 404s turns the
// whole channel into model_unavailable ("switch models"), which is the wrong
// advice. It must be its own class with no account penalty.
func TestNotFoundIsNotModelRate(t *testing.T) {
	ue := classifyHTTP(http.StatusNotFound, []byte(`404 page not found`), "glm-5.3")
	if ue == nil {
		t.Fatal("classifyHTTP returned nil")
	}
	if ue.Class == uperr.ClassModelRate {
		t.Fatalf("404 must not be ClassModelRate (cools the account's model); got class=%s", ue.Class)
	}
	if ue.Class != uperr.ClassNotFound {
		t.Fatalf("404 class = %s, want %s", ue.Class, uperr.ClassNotFound)
	}
	if !uerr_NoPenalty(ue.Class) {
		t.Fatalf("404 must not penalize the account")
	}
}

// TestPromptTooLongIsRequestLevel: the upstream expresses an over-context
// prompt with 400/404/413 + code 11115 (or the literal wording). The remedy is
// trimming the request, not rotating accounts — the same body fails on every
// account. This must be caught BEFORE the status-code fallbacks, because a 404
// carrying 11115 would otherwise look like a routing/model problem.
func TestPromptTooLongIsRequestLevel(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 with code", http.StatusBadRequest, `{"code":11115,"msg":"prompt is too long"}`},
		{"413 with code", http.StatusRequestEntityTooLarge, `{"code":11115,"msg":"too long"}`},
		{"404 with code", http.StatusNotFound, `{"code":11115,"msg":"prompt is too long"}`},
		{"400 with wording only", http.StatusBadRequest, `{"error":{"message":"Prompt is too long for this model"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ue := classifyHTTP(tc.status, []byte(tc.body), "glm-5.3")
			if ue.Class != uperr.ClassBadRequest {
				t.Fatalf("class = %s, want %s (request-level, no account penalty)",
					ue.Class, uperr.ClassBadRequest)
			}
			// The business-code path normalizes to 400 (the envelope carries
			// no transport semantics of its own). What matters is that it is
			// a 4xx the client can act on, never a 5xx implying a gateway bug.
			if ue.Status < 400 || ue.Status > 499 {
				t.Fatalf("status = %d, want a 4xx request-level code", ue.Status)
			}
		})
	}
}

// A plain 413 without the 11115 marker is still request-level: the payload is
// too large, and no account rotation can fix it.
func TestPayloadTooLargeIsRequestLevel(t *testing.T) {
	ue := classifyHTTP(http.StatusRequestEntityTooLarge, []byte(`payload too large`), "glm-5.3")
	if ue.Class != uperr.ClassBadRequest {
		t.Fatalf("413 class = %s, want %s", ue.Class, uperr.ClassBadRequest)
	}
}

// uerr_NoPenalty mirrors the pool's no-penalty set from the taxonomy side: a
// class is penalty-free when the pool makes no account-state change for it.
func uerr_NoPenalty(class string) bool {
	switch class {
	case uperr.ClassBadRequest, uperr.ClassContentBlocked, uperr.ClassSentinel,
		uperr.ClassUpstreamTimeout, uperr.ClassCanceled, uperr.ClassUpstreamBusy,
		uperr.ClassNotFound:
		return true
	}
	return false
}
