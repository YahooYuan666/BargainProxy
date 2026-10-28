package trae

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-173: Kimi-routed models get their parameters type-normalized — an
// enum-only property (no type) used to reach Trae verbatim and answer 4027
// "not a valid moonshot flavored json schema".
func TestBP173KimiSchemaNormalized(t *testing.T) {
	call := driver.ChatCall{
		Account:       &model.Account{ID: "tr-ks", Channel: model.ChannelTrae},
		UpstreamModel: "kimi-k3",
		Request: &ir.ChatRequest{
			Model: "kimi-k3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Tools: []ir.ToolDef{{
				Name: "pick_color",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"color": map[string]any{"enum": []string{"red", "blue"}},
					},
				},
			}},
		},
	}
	raw, err := buildSOLOBody(call, "solo_work_lite")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tools []struct {
			Function struct {
				Parameters string `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tools) != 1 {
		t.Fatalf("tools = %d", len(doc.Tools))
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(doc.Tools[0].Function.Parameters), &params); err != nil {
		t.Fatal(err)
	}
	props := params["properties"].(map[string]any)
	color := props["color"].(map[string]any)
	if color["type"] != "string" {
		t.Fatalf("enum-only node type = %v, want string", color["type"])
	}

	// Non-Kimi models keep the schema untouched.
	glm := call
	glm.UpstreamModel = "glm-5.3"
	glm.Request = &ir.ChatRequest{
		Model: "glm-5.3",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		Tools: []ir.ToolDef{{
			Name: "pick_color",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"color": map[string]any{"enum": []string{"red", "blue"}}},
			},
		}},
	}
	raw2, _ := buildSOLOBody(glm, "solo_work_lite")
	var doc2 struct {
		Tools []struct {
			Function struct {
				Parameters string `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(raw2, &doc2)
	var params2 map[string]any
	_ = json.Unmarshal([]byte(doc2.Tools[0].Function.Parameters), &params2)
	props2 := params2["properties"].(map[string]any)
	color2 := props2["color"].(map[string]any)
	if _, has := color2["type"]; has {
		t.Fatalf("non-Kimi schema must pass untouched, got %v", color2["type"])
	}
}

// BP-183: the metadata frames name what actually served — a mismatch warns.
func TestBP183BilledModelExtracted(t *testing.T) {
	if got := billedModelOf(`{"extra_info":{"model":"deepseek-v4.1-flash"}}`); got != "deepseek-v4.1-flash" {
		t.Fatalf("extra_info.model = %q", got)
	}
	if got := billedModelOf(`{"timing_cost":{"provider_model_name":"ali-kimi-k3"}}`); got != "ali-kimi-k3" {
		t.Fatalf("timing_cost.provider_model_name = %q", got)
	}
	if got := billedModelOf(`{"choices":[]}`); got != "" {
		t.Fatalf("ordinary frame = %q, want empty", got)
	}
}
