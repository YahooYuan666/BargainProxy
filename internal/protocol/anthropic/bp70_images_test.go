package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func toolResultBody(images, texts int) string {
	var blocks []string
	for i := 0; i < texts; i++ {
		blocks = append(blocks, `{"type":"text","text":"log line"}`)
	}
	for i := 0; i < images; i++ {
		blocks = append(blocks, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`)
	}
	return `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"take a screenshot"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` + strings.Join(blocks, ",") + `]}]}
	]}`
}

// BP-70: tool-result images must survive parsing as image parts (previously
// only text was collected, so MCP screenshot tools were invisible upstream).
func TestParseToolResultKeepsImages(t *testing.T) {
	req, err := ParseMessagesRequest([]byte(toolResultBody(1, 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var tool *ir.Message
	for i := range req.Messages {
		if req.Messages[i].Role == ir.RoleTool {
			tool = &req.Messages[i]
		}
	}
	if tool == nil {
		t.Fatal("tool message missing")
	}
	var images, texts int
	for _, p := range tool.Content {
		switch p.Kind {
		case "image":
			images++
			if p.ImageB64 != "QUJD" || p.ImageMIME != "image/png" {
				t.Fatalf("image part = %+v", p)
			}
		case "text":
			texts++
		}
	}
	if images != 1 || texts != 1 {
		t.Fatalf("images=%d texts=%d", images, texts)
	}
}

// The budget keeps the most recent five images and never touches text.
func TestToolResultImageBudget(t *testing.T) {
	req, err := ParseMessagesRequest([]byte(toolResultBody(7, 2)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var images, texts int
	for _, m := range req.Messages {
		if m.Role != ir.RoleTool {
			continue
		}
		for _, p := range m.Content {
			switch p.Kind {
			case "image":
				images++
			case "text":
				texts++
			}
		}
	}
	if images != maxToolResultImages {
		t.Fatalf("images = %d, want %d", images, maxToolResultImages)
	}
	if texts != 2 {
		t.Fatalf("texts = %d, want 2 (text must survive the cap)", texts)
	}
}

func TestToolResultBudgetKeepsNewest(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"a","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OLD1"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OLD2"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OLD3"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OLD4"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"NEW1"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"NEW2"}}
		]}]}
	]}`
	req, err := ParseMessagesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data []string
	for _, m := range req.Messages {
		if m.Role != ir.RoleTool {
			continue
		}
		for _, p := range m.Content {
			if p.Kind == "image" {
				data = append(data, p.ImageB64)
			}
		}
	}
	// Six images in, keep the newest five (OLD1 is dropped), order preserved.
	want := []string{"OLD2", "OLD3", "OLD4", "NEW1", "NEW2"}
	if strings.Join(data, ",") != strings.Join(want, ",") {
		t.Fatalf("kept = %v, want %v (newest first, order preserved)", data, want)
	}
}

func TestToolResultImageURLStillRejected(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"a","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}
		]}]}
	]}`
	req, err := ParseMessagesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Kind == "image_url_unsupported" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("external tool-result image URL must be marked unsupported")
	}
}

// A tool result without any text blocks must still be a valid text message,
// otherwise OpenAI-shaped drivers would emit an empty string and some
// upstreams reject the turn.
func TestToolResultImageOnlyHasNoDanglingParts(t *testing.T) {
	req, err := ParseMessagesRequest([]byte(`{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"a","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[]}]}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role == ir.RoleTool {
			if len(m.Content) != 1 || m.Content[0].Kind != "text" {
				t.Fatalf("empty tool result = %+v", m.Content)
			}
			return
		}
	}
	t.Fatal("tool message missing")
}

func TestToolResultStringContentUnchanged(t *testing.T) {
	req, err := ParseMessagesRequest([]byte(`{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"a","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"all good"}]}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var raw []byte
	for _, m := range req.Messages {
		if m.Role == ir.RoleTool {
			raw, _ = json.Marshal(m.Content)
		}
	}
	if !strings.Contains(string(raw), `"all good"`) {
		t.Fatalf("string content lost: %s", raw)
	}
}
