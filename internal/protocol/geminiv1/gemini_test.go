package geminiv1

import (
	"bytes"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func TestParseGenerateRequest(t *testing.T) {
	body := []byte(`{
	  "systemInstruction":{"parts":[{"text":"sys"}]},
	  "contents":[
	    {"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},
	    {"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":1}}}]},
	    {"role":"function","parts":[{"functionResponse":{"name":"f","response":{"content":"ok"}}}]}
	  ],
	  "tools":[{"functionDeclarations":[{"name":"f","description":"d","parameters":{"type":"object"}}]}],
	  "generationConfig":{"temperature":0.5,"maxOutputTokens":50,"thinkingConfig":{"thinkingBudget":1024}},
	  "toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}
	}`)
	req, err := ParseRequest(body, "xid:sess-1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.SessionKey != "xid:sess-1" {
		t.Fatalf("session = %q", req.SessionKey)
	}
	if len(req.Messages) != 4 { // system + user + model + function
		t.Fatalf("messages = %d: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != ir.RoleUser || len(req.Messages[1].Content) != 2 {
		t.Fatalf("user parts: %+v", req.Messages[1])
	}
	if req.Messages[1].Content[1].Kind != "image" || req.Messages[1].Content[1].ImageMIME != "image/png" {
		t.Fatalf("image part: %+v", req.Messages[1].Content[1])
	}
	if req.Messages[2].Role != ir.RoleAssistant || len(req.Messages[2].ToolCalls) != 1 {
		t.Fatalf("model tool call: %+v", req.Messages[2])
	}
	if req.Messages[3].Role != ir.RoleTool || req.Messages[3].ToolName != "f" {
		t.Fatalf("function response: %+v", req.Messages[3])
	}
	if req.Temperature == nil || *req.Temperature != 0.5 || req.MaxTokens != 50 {
		t.Fatalf("generation config: %+v", req)
	}
	if req.Thinking == nil || req.Thinking.BudgetTokens != 1024 {
		t.Fatalf("thinking: %+v", req.Thinking)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" {
		t.Fatalf("tools: %+v", req.Tools)
	}
}

func TestStreamFramesNoDoneSentinel(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "gemini-3-pro", false)
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvText, Delta: "Hel"},
		{Kind: ir.EvText, Delta: "lo"},
		{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 1, OutputTokens: 2}},
		{Kind: ir.EvDone, Stop: ir.StopEnd},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatal("Gemini streams must not carry [DONE]")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var dataLines []string
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") {
			dataLines = append(dataLines, l)
		}
	}
	if len(dataLines) != 3 { // Hel, lo, final(usage+finish)
		t.Fatalf("frames = %d: %v", len(dataLines), dataLines)
	}
	last := dataLines[len(dataLines)-1]
	if !strings.Contains(last, `"finishReason":"STOP"`) {
		t.Fatalf("final frame missing finishReason: %s", last)
	}
	if !strings.Contains(last, `"usageMetadata"`) {
		t.Fatalf("final frame missing usage: %s", last)
	}
}

func TestRenderShape(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopLength,
		Usage:      ir.Usage{InputTokens: 3, OutputTokens: 9},
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "x"}}},
		Thinking:   "why",
	}
	out := Render("gemini-3-pro", resp, false)
	if out.Candidates[0].Content.Role != "model" || out.Candidates[0].FinishReason != "MAX_TOKENS" {
		t.Fatalf("candidate: %+v", out.Candidates[0])
	}
	if out.UsageMetadata.TotalTokenCount != 12 {
		t.Fatalf("usage: %+v", out.UsageMetadata)
	}
	// includeThoughts=false must not leak thought parts (BP-41).
	for _, p := range out.Candidates[0].Content.Parts {
		if p.Thought {
			t.Fatalf("thought part leaked without opt-in: %+v", p)
		}
	}
	// Opt-in renders the aggregated thinking as a thought part up front.
	withThoughts := Render("gemini-3-pro", resp, true)
	if p := withThoughts.Candidates[0].Content.Parts[0]; !p.Thought || p.Text != "why" {
		t.Fatalf("thought part missing on opt-in: %+v", withThoughts.Candidates[0].Content.Parts)
	}
}

// BP-41: thought parts are only streamed when the client opted in via
// includeThoughts; the text frames stay intact either way.
func TestStreamThoughtPartsGatedByIncludeThoughts(t *testing.T) {
	for _, inc := range []bool{false, true} {
		var buf bytes.Buffer
		sw := NewStreamWriter(&buf, nil, "gemini-3.8-flash-high", inc)
		for _, ev := range []ir.StreamEvent{
			{Kind: ir.EvThinking, Delta: "想"},
			{Kind: ir.EvText, Delta: "答"},
			{Kind: ir.EvDone, Stop: ir.StopEnd},
		} {
			if err := sw.Send(ev); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		out := buf.String()
		hasThought := strings.Contains(out, `"thought":true`)
		if inc != hasThought {
			t.Fatalf("includeThoughts=%v → thought part present=%v, out=%s", inc, hasThought, out)
		}
		if !strings.Contains(out, `"答"`) {
			t.Fatalf("text frame missing: %s", out)
		}
	}
}

// BP-41: the ingress reads includeThoughts into ThinkingConfig.Include.
func TestParseRequestIncludeThoughts(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}`)
	req, err := ParseRequest(body, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || !req.Thinking.Enabled || req.Thinking.Level != "high" || !req.Thinking.Include {
		t.Fatalf("thinking = %+v, want enabled level=high include=true", req.Thinking)
	}
}
