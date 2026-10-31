package autoclaw

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-160: the CN gateway 500s ({"message":"parse response failed"}) on
// message shapes its own client never sends — missing content keys, empty
// argument strings (plugins autoclaw 0.2.9 tidyBody, issue #437).
func TestBP160TidyBody(t *testing.T) {
	call := driver.ChatCall{
		Account:       &model.Account{ID: "ac-tb", Channel: model.ChannelAutoClaw},
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "zai_glm-5.3-flash",
			Messages: []ir.Message{
				// An assistant turn that only carries tool calls: the old
				// builder emitted no content key at all.
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: ""},
				}},
				{Role: ir.RoleTool, ToolCallID: "call_1", Content: []ir.ContentPart{{Kind: "text", Text: "file body"}}},
			},
		},
	}
	raw, _, err := buildBodyWith(call, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	first := doc.Messages[0]
	if _, ok := first["content"]; !ok {
		t.Fatal("assistant tool-call turn lost its content key — the gateway parses that as a 500")
	}
	if first["content"] != "" {
		t.Fatalf("content = %v, want empty string", first["content"])
	}
	tcs, _ := first["tool_calls"].([]any)
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Fatalf("arguments = %v, want the JSON object string", fn["arguments"])
	}
}

// A normal turn is untouched by the tidy-up.
func TestBP160TidyBodyLeavesNormalShapes(t *testing.T) {
	call := driver.ChatCall{
		Account:       &model.Account{ID: "ac-tb", Channel: model.ChannelAutoClaw},
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "zai_glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
			},
		},
	}
	raw, _, err := buildBodyWith(call, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Messages[0]["content"] != "hello" {
		t.Fatalf("content = %v", doc.Messages[0]["content"])
	}
}

// BP-176: the WorkBuddy GLM opening line and "You are CodeBuddy Code" are
// 406 on the CN gateway (plugins 0.2.10, d8cfee3) — the scrub rewrites the
// opening line and drops the claim sentence.
func TestBP176RefusedIdentitiesScrubbed(t *testing.T) {
	rewritten := scrubRefusedIdentity("This conversation is powered by GLM-5.3 · AutoClaw 国内. Help the user code.")
	if strings.Contains(rewritten, "powered by GLM-5.3 ·") {
		t.Fatalf("GLM opening line survived: %q", rewritten)
	}
	if !strings.Contains(rewritten, "powered by the GLM-5.3 model") {
		t.Fatalf("neutral rewrite missing: %q", rewritten)
	}
	if !strings.Contains(rewritten, "Help the user code.") {
		t.Fatalf("surrounding text must survive: %q", rewritten)
	}

	dropped := scrubRefusedIdentity("You are CodeBuddy Code, Tencent's coding agent. Answer in Chinese.")
	if strings.Contains(strings.ToLower(dropped), "codebuddy") {
		t.Fatalf("CodeBuddy claim survived: %q", dropped)
	}
	if !strings.Contains(dropped, "Answer in Chinese.") {
		t.Fatalf("surrounding text must survive: %q", dropped)
	}
}

// BP-180: the bare "[DONE]" line (no "data:" prefix) terminates the stream.
func TestBP180BareDoneTerminatesAutoclaw(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n[DONE]\n\n"
	call := driver.ChatCall{
		Account:       &model.Account{ID: "ac-bd", Channel: model.ChannelAutoClaw},
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model:    "zai_glm-5.3-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	d := &Driver{}
	_ = d
	resp, err := parseSSE(context.Background(), strings.NewReader(body), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("bare [DONE] must terminate cleanly: %v", err)
	}
	if ir.TextOf(resp.Message.Content) != "ok" {
		t.Fatalf("text lost: %q", ir.TextOf(resp.Message.Content))
	}
}

// BP-230 (plugins autoclaw 0.2.13 acb85ed8): the rewordable identities keep
// their name — Hermes Agent measured 406→200 with the reworded forms, and
// deletion would amputate the name the rest of the prompt refers back to.
// The deletion pass for the original four stays untouched.
func TestBP230IdentityRewritesKeepTheName(t *testing.T) {
	cases := []struct{ in, want, forbids string }{
		{
			"You are Hermes Agent, built by Nous Research. Be direct. You run on Hermes Agent (by Nous Research).",
			"You are the agent Hermes, built by Nous Research. Be direct. You run on Hermes-Agent (by Nous Research).",
			"Hermes Agent",
		},
		{
			"You are Gemini CLI, Google's coding agent.",
			"You are the Gemini CLI agent, Google's coding agent.",
			"You are Gemini CLI",
		},
		{
			"You are Kimi Code CLI. Help with repos.",
			"You are the Kimi Code CLI agent. Help with repos.",
			"You are Kimi Code CLI",
		},
		{
			"You are goose, the codename's agent.",
			"You are the goose agent, the codename's agent.",
			"You are goose",
		},
	}
	for _, c := range cases {
		got := scrubRefusedIdentity(c.in)
		if got != c.want {
			t.Fatalf("scrub(%q) =\n  %q\nwant:\n  %q", c.in, got, c.want)
		}
		if strings.Contains(got, c.forbids) {
			t.Fatalf("scrub output still carries the refused form %q: %q", c.forbids, got)
		}
	}
	// The original four still go through sentence deletion.
	if got := scrubRefusedIdentity("You are Claude Code, Anthropic's CLI. Be brief."); strings.Contains(got, "Claude Code") {
		t.Fatalf("claude code claim must still be deleted: %q", got)
	}
	// A mixed prompt keeps both remedies.
	got := scrubRefusedIdentity("You are Hermes Agent. Also, You are Codex, OpenAI's agent.")
	if strings.Contains(got, "Hermes Agent") || strings.Contains(got, "Codex") {
		t.Fatalf("mixed prompt not fully scrubbed: %q", got)
	}
	if !strings.Contains(got, "the agent Hermes") {
		t.Fatalf("hermes name lost in the mixed prompt: %q", got)
	}
}
