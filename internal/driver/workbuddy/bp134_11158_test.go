package workbuddy

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-134: live 2026-10-27 (scripts/wbclaude, CN account + CN host) — the CN
// deployment's catalog lists NO claude models, and both lanes refuse
// claude-opus-5.5 with 11158 "not allowed for user type [personal_free_user]".
// The /v1/messages envelope wraps it as code 11102 with the 11158 text in the
// flat error string. The account is fine on its other models: model-scoped
// hold, never internal, never an account penalty.
func Test11158ModelNotAllowedIsModelScoped(t *testing.T) {
	// chat-lane shape
	e := classifyBusiness(11158, "model [claude-opus-5.5] is not allowed for user type [personal_free_user]", "claude-opus-5.5")
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("chat 11158 class = %s, want model_rate", e.Class)
	}
	if e.Model != "claude-opus-5.5" || e.Code != "11158" {
		t.Fatalf("model=%q code=%q", e.Model, e.Code)
	}
	// /v1/messages envelope shape: 11102 + flat error string
	code, msg, ok := detectBusinessError([]byte(`{"code":11102,"error":"11158:model [claude-opus-5.5] is not allowed for user type [personal_free_user]","type":"invalid_request_error"}`))
	if !ok {
		t.Fatal("11102 envelope must be detected")
	}
	e2 := classifyBusiness(code, msg, "claude-opus-5.5")
	if e2.Class != uperr.ClassModelRate || e2.Code != "11158" {
		t.Fatalf("envelope class = %s code = %q, want model_rate/11158", e2.Class, e2.Code)
	}
	// Unrelated codes keep their old paths.
	if e3 := classifyBusiness(11101, "invalid params", "glm-5.3"); e3.Class != uperr.ClassBadRequest {
		t.Fatalf("11101 class = %s, want bad_request", e3.Class)
	}
}
