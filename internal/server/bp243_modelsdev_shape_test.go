package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"bargainproxy/internal/config"
)

// BP-243: /v1/models must carry the models.dev vocabulary ALONGSIDE our own.
//
// A client that reads models.dev keys (magpie, the OpenCode family) currently
// sees nothing usable and degrades every model to text-only. The fix appends
// modalities/limit/tool_call/reasoning_options without touching a single key we
// already publish, and without inventing values for models we have no spec for.
func TestModelsPublishModelsDevShape(t *testing.T) {
	srv, _ := buildModelsServer(t)
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) == 0 {
		t.Fatal("no models listed")
	}

	var found map[string]any
	for _, m := range body.Data {
		if m["id"] == "glm-5.2" {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatalf("glm-5.2 missing from %v", body.Data)
	}

	// The models.dev keys are present.
	if _, ok := found["limit"]; !ok {
		t.Error("limit missing: a models.dev client cannot see the ceilings")
	}
	mods, ok := found["modalities"].(map[string]any)
	if !ok {
		t.Fatal("modalities missing: a models.dev client reads the model as text-only")
	}
	if in, _ := mods["input"].([]any); len(in) == 0 {
		t.Error("modalities.input empty")
	}
	if found["tool_call"] != true {
		t.Errorf("tool_call = %v, want true for glm-5.2", found["tool_call"])
	}
	if _, ok := found["reasoning_options"]; !ok {
		t.Error("reasoning_options missing for a model with gears")
	}

	// Purely additive: the keys we already published are untouched.
	if found["id"] != "glm-5.2" || found["object"] != "model" || found["owned_by"] == nil {
		t.Errorf("existing keys changed: %v", found)
	}
	if _, ok := found["context_length"]; !ok {
		t.Error("context_length disappeared; the old dialect must survive")
	}
}

// A model with no spec row must get NO capability keys. Inventing them is worse
// than omitting them, because clients branch on presence.
func TestModelsShapeOmittedWithoutSpec(t *testing.T) {
	spec, ok := config.ModelSpecFor("no-such-model-xyz", nil)
	if ok {
		t.Fatalf("a model with no spec row resolved to %+v", spec)
	}
}
