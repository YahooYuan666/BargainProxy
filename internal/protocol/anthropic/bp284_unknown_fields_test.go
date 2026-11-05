package anthropic

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// BP-284: a request field the gateway cannot honour must be named in the log.
//
// The case that prompted it: ZCode sends output_config:{effort:"max"} beside a
// thinking block that names no budget. The gateway derives the gear from the
// budget, finds none, and falls back to "high" — so the client's "max" is
// silently downgraded and every symptom points at the model instead of at the
// gateway that dropped the field.
//
// BP-286 changed the example: output_config is now read, so the sentinel is a
// field the gateway genuinely does not implement. output_config staying
// warning-free is asserted in bp286_effort_test.go.
func TestUnknownFieldIsNamed(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	body := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":10,
		"thinking":{"type":"enabled"},
		"output_config":{"effort":"max"},
		"future_field_we_do_not_implement":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := ParseMessagesRequest(body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "future_field_we_do_not_implement") {
		t.Fatalf("the dropped field was not named in the log: %q", got)
	}
	if strings.Contains(got, "output_config") {
		t.Fatalf("output_config is implemented now and must not be reported as dropped: %q", got)
	}
}

// A request that only uses fields the gateway implements must stay quiet —
// otherwise the warning becomes noise and stops being read.
func TestKnownFieldsStayQuiet(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	body := []byte(`{"model":"glm-5.3","max_tokens":10,"stream":true,
		"temperature":0.3,"top_p":0.9,"top_k":5,
		"thinking":{"type":"enabled","budget_tokens":4000},
		"metadata":{"user_id":"u1"},
		"tools":[{"name":"t","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto"},
		"stop_sequences":["END"],
		"system":"be brief",
		"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := ParseMessagesRequest(body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "ignoring request field") {
		t.Fatalf("a fully supported request must not warn: %q", got)
	}
}

// Every field the warning has to tolerate is one the parser actually reads.
// This guards the allowlist against drifting out of sync with Request.
func TestUnknownFieldWarningDoesNotReject(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	body := []byte(`{"model":"m","max_tokens":5,"some_future_field":{"a":1},
		"messages":[{"role":"user","content":"hi"}]}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("an unknown field must not fail the parse: %v", err)
	}
	if req.Model != "m" {
		t.Fatalf("the rest of the request was lost: %+v", req)
	}
	if !strings.Contains(buf.String(), "some_future_field") {
		t.Fatalf("unknown field not named: %q", buf.String())
	}
}
