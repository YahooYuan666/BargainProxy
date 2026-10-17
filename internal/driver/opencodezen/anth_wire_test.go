package opencodezen

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/protocol/anthropic"
)

// Integration between the Anthropic ingress and the chat-lane wire: tool
// declarations must survive parsing and reach the upstream (a dropped tools
// array makes the model answer "no tools available" instead of calling).
func TestAnthropicToolsReachTheWire(t *testing.T) {
	body := []byte(`{
	  "model":"space-bunny-free","max_tokens":256,
	  "system":"You are a weather assistant.",
	  "tools":[{"name":"get_weather","description":"Get current weather for a city",
	    "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
	  "messages":[{"role":"user","content":"Weather in SF? Use get_weather."}]
	}`)
	req, err := anthropic.ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("parsed tools = %+v", req.Tools)
	}
	d := New(config.OpenCodeZenSettings{}, nil)
	before, _ := json.Marshal(req.Tools)
	t.Logf("before build: %d tools %s", len(req.Tools), before)
	wire, err := d.buildWire(driver.ChatCall{UpstreamModel: "space-bunny-free", Request: req})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	after, _ := json.Marshal(req.Tools)
	t.Logf("after build: %d tools %s", len(req.Tools), after)
	var got map[string]any
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := got["tools"].([]any)
	names := []string{}
	for _, tl := range tools {
		if tm, ok := tl.(map[string]any); ok {
			if fn, ok := tm["function"].(map[string]any); ok {
				names = append(names, fn["name"].(string))
			}
		}
	}
	t.Logf("wire tools = %v; body = %s", names, wire)
	if !strings.Contains(strings.Join(names, ","), "get_weather") {
		t.Fatalf("tool declaration lost on the wire: %v", names)
	}
	_ = ir.RoleUser
}
