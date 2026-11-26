package antigravity

// BP-340: the Gemini 4 band gates on the credential's own model enum plus the
// agent envelope shape. These tests pin the parts that are pure logic; the live
// proof (enum probe → 200 with text) was run against a real Pro account on
// 2026-11-25 and is recorded in catalog.go's file comment.

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func TestBP340AgentRequestIDShape(t *testing.T) {
	ids := newAgentIDs(3)
	rid := ids.requestID()
	parts := strings.Split(rid, "/")
	if len(parts) != 5 {
		t.Fatalf("requestId %q has %d segments, want 5", rid, len(parts))
	}
	if parts[0] != "agent" {
		t.Fatalf("requestId %q must start with the agent kind", rid)
	}
	if parts[1] != ids.cascade || parts[3] != ids.trajectory {
		t.Fatalf("requestId %q does not embed cascade/trajectory", rid)
	}
	if parts[4] != "3" {
		t.Fatalf("requestId %q step = %q, want 3", rid, parts[4])
	}
	// The two ids must be distinct UUIDv4s (the upstream rejected a bare hex pair).
	if ids.cascade == ids.trajectory {
		t.Fatal("cascade and trajectory must differ")
	}
	for _, u := range []string{ids.cascade, ids.trajectory} {
		if len(u) != 36 || u[14] != '4' {
			t.Fatalf("%q is not a UUIDv4", u)
		}
	}
}

func TestBP340LabelsCarryModelEnum(t *testing.T) {
	ids := newAgentIDs(1)
	l := ids.labels("gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M525")
	want := map[string]string{
		"cascade_id":               ids.cascade,
		"root_cascade_id":          ids.cascade,
		"trajectory_id":            ids.trajectory,
		"last_step_index":          "1",
		"used_claude":              "false",
		"used_claude_conservative": "false",
		"used_non_gemini_model":    "false",
		"model_enum":               "MODEL_PLACEHOLDER_M525",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	if !strings.HasPrefix(l["request_id"], ids.trajectory+"-") {
		t.Errorf("request_id label = %q", l["request_id"])
	}

	// Claude turns flip the flags.
	cl := ids.labels("claude-sonnet-4-6", "")
	if cl["used_claude"] != "true" || cl["used_non_gemini_model"] != "true" {
		t.Errorf("claude flags wrong: %v", cl)
	}
	// Unknown enum is omitted, never guessed.
	if _, ok := cl["model_enum"]; ok {
		t.Error("model_enum must be absent when the catalog has no answer")
	}
}

// The live path folds labels into request.labels and renders the agent
// requestId; the historical 4-arg entry point still produces the old shape so
// existing callers/tests stay valid.
func TestBP340EnvelopeReachesRequestBody(t *testing.T) {
	req := &ir.ChatRequest{Messages: []ir.Message{{
		Role: "user", Content: []ir.ContentPart{{Kind: "text", Text: "hi"}},
	}}}
	ids := newAgentIDs(2)
	env := &requestEnvelope{
		RequestID: ids.requestID(),
		Labels:    ids.labels("gemini-4-hydrogen", "MODEL_PLACEHOLDER_M525"),
	}
	raw, err := buildRequestWithEnvelope("proj", "gemini-4-hydrogen-high", req, nil, env)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		RequestID string `json:"requestId"`
		Request   struct {
			Labels map[string]string `json:"labels"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.RequestID != env.RequestID {
		t.Errorf("requestId = %q", doc.RequestID)
	}
	if doc.Request.Labels["model_enum"] != "MODEL_PLACEHOLDER_M525" {
		t.Errorf("labels = %v", doc.Request.Labels)
	}
	if doc.Request.Labels["cascade_id"] != ids.cascade {
		t.Errorf("cascade_id missing: %v", doc.Request.Labels)
	}
}

// The 4-arg wrapper still works and still stamps a valid agent envelope — the
// envelope is structural, only the enum is optional.
func TestBP340BareWrapperStillBuilds(t *testing.T) {
	req := &ir.ChatRequest{Messages: []ir.Message{{
		Role: "user", Content: []ir.ContentPart{{Kind: "text", Text: "hi"}},
	}}}
	raw, err := buildRequest("proj", "gemini-3.8-flash-high", req, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc["requestId"].(string), "agent/") {
		t.Fatalf("requestId = %v", doc["requestId"])
	}
}
