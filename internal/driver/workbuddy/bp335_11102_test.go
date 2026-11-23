// BP-335: bare 11102 ("model [X] service info not found") is REQUEST-level.
// The model id does not exist upstream, so every account answers the same
// thing — walking the pool writes a bogus (account, model) negative cache on
// each and ends with the backwards advice "retry on another account".
//
// misakano d629a8ca (2026-11-21) measured DeepSeek-V4.1-Flash going from
// 400/4.15s/try=3 to 400/0.19s/try=1 with the split. 11158 (model not allowed
// for this account TYPE) keeps its model-scoped hold: a Pro account really can
// serve that model.
package workbuddy

import (
	"strings"
	"testing"

	"bargainproxy/internal/uperr"
)

func TestBare11102IsRequestLevel(t *testing.T) {
	e := classifyBusiness(11102, "model [DeepSeek-V4.1-Flash] service info not found", "deepseek-v4.1-flash")
	if e.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %s, want bad_request (request-level)", e.Class)
	}
	if uperr.Rotatable(e.Class) {
		t.Fatalf("11102 must NOT rotate: every account answers the same thing")
	}
	if e.Model != "" {
		t.Fatalf("11102 must not write a model-scoped cooldown (e.Model = %q)", e.Model)
	}
}

// The client-facing hint must say switching accounts cannot help — the old
// wording pointed the wrong way.
func Test11102HintSaysSwitchingWillNotHelp(t *testing.T) {
	e := classifyBusiness(11102, "model [x] service info not found", "x")
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "will not help") && !strings.Contains(low, "换号") {
		t.Fatalf("11102 hint must say switching accounts cannot help: %q", e.Message)
	}
}

// 11158 stays model-scoped: another account type CAN serve the model.
func Test11158StaysModelScoped(t *testing.T) {
	e := classifyBusiness(11158, "model claude-sonnet-4-6 is not allowed for user type", "claude-sonnet-4-6")
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("class = %s, want model_rate", e.Class)
	}
	if e.Model != "claude-sonnet-4-6" {
		t.Fatalf("11158 must hold the model, got %q", e.Model)
	}
}

// The /v1/messages envelope wraps 11158 as code 11102 with "11158:…" in the
// text. That wrapper must keep its model-scoped verdict — the split above must
// not capture it.
func TestMessagesEnvelope11102With11158TextStaysModelScoped(t *testing.T) {
	e := classifyBusiness(11102, "11158:model claude-sonnet-4-6 is not allowed for user type", "claude-sonnet-4-6")
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("the 11158 wrapper must stay model-scoped, got %s", e.Class)
	}
	if e.Code != "11158" {
		t.Fatalf("the wrapper must keep code 11158, got %q", e.Code)
	}
}
