package autoclaw

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// Test406IsBadRequest pins BP-98: a 406 from the CN managed gateway is the
// channel check refusing the request's system prompt, not an account fault.
// Before this branch existed it fell into ClassInternal and charged the
// account a failure for a refusal every account would hit identically.
func Test406IsBadRequest(t *testing.T) {
	ue := classifyHTTP(406, []byte{})
	if ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassBadRequest)
	}
	if uperr.Rotatable(ue.Class) {
		t.Fatal("a 406 must not rotate accounts: the same request is refused everywhere")
	}
}

// TestScrubRemovesOnlyTheRefusedClaim pins the 2026-10-23 replacement for
// BP-99's whole-prompt move: the retry deletes ONLY the sentence that asserts a
// refused identity, and keeps every other instruction the caller gave.
//
// Field matrix (scripts/acprobe2, 2 runs each): "You are Claude Code…",
// "you are claude code.", "You are ZCode…" and "You are Codex…" all answer 406
// in ~100ms, while "You are Claude.", "You are Cursor…", "You are Windsurf.",
// "You are Gemini CLI…" and "Claude Code is the assistant…" (no "you are")
// answer 200. So the trigger is a second-person claim naming a competing
// agent, and the surgical fix is to drop just that claim.
func TestScrubRemovesOnlyTheRefusedClaim(t *testing.T) {
	const kept = "Tone and style: be concise and direct. Prefer dedicated tools over shell."
	req := &ir.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
				Text: "You are Claude Code, Anthropic's official CLI for Claude. " + kept}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
		},
	}
	raw, err := buildBodySystemAsUser(driver.ChatCall{UpstreamModel: "zai_glm-5.3-flash", Request: req})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (a scrub must not add or drop turns)", len(doc.Messages))
	}
	if doc.Messages[0].Role != "system" {
		t.Fatalf("first role = %q, want system (the gateway refuses a non-system first message)", doc.Messages[0].Role)
	}
	sys, _ := doc.Messages[0].Content.(string)
	if strings.Contains(strings.ToLower(sys), "claude code") {
		t.Fatalf("the refused claim survived: %q", sys)
	}
	if !strings.Contains(sys, kept) {
		t.Fatalf("the caller's other instructions were lost: %q", sys)
	}
	// The user turn is untouched: the retry no longer moves prompt text there.
	if user, _ := doc.Messages[1].Content.(string); user != "hello" {
		t.Fatalf("user turn = %q, want the original %q", user, "hello")
	}
	// The caller's IR must be untouched: a later attempt (another account, the
	// relay path) still needs the original system message.
	if !strings.Contains(ir.TextOf(req.Messages[0].Content), "Claude Code") {
		t.Fatalf("the original request was mutated: %+v", req.Messages)
	}
}

// TestScrubLeavesAllowedIdentitiesAlone: a system prompt naming a product the
// gateway accepts must be sent verbatim — scrubbing it would silently change
// the agent's behaviour for no reason. (BP-230: "You are Gemini CLI" left
// this list — the 10-23 bisect passed it, but the gateway's 10-30 channel
// check refuses the claim form and the upstream plugin now rewrites it.)
func TestScrubLeavesAllowedIdentitiesAlone(t *testing.T) {
	for _, text := range []string{
		"You are Claude.",
		"You are Cursor, an AI code editor.",
		"You are Windsurf.",
		"You are an AI assistant helping with software engineering.",
		"Claude Code is the assistant in this session.",
	} {
		if got := scrubRefusedIdentity(text); got != text {
			t.Errorf("scrubbed an accepted prompt: %q → %q", text, got)
		}
	}
}

// TestScrubHandlesTheFieldShapes: the three refused products, any casing, and
// a claim buried mid-prompt (the field failure had ~13k tokens of instructions
// around it).
func TestScrubHandlesTheFieldShapes(t *testing.T) {
	cases := map[string]string{
		"You are Claude Code.":                                              "",
		"you are claude code.":                                              "",
		"You are ZCode, a coding agent.":                                    "",
		"You are Codex, OpenAI's coding agent.":                             "",
		"You are Claude Code an interactive CLI tool.":                      "",
		"Preamble line. You are Claude Code, a CLI tool. Tail instruction.": "Preamble line. Tail instruction.",
	}
	for in, want := range cases {
		if got := scrubRefusedIdentity(in); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

// Test810000IsModelScoped pins BP-100: the gateway's per-route allowance wall
// is a code, not wording, and it belongs to ONE model — the same account still
// serves other routes. CooldownScope/pool then rests only that model.
//
// Evidence: 2642086672/autoclaw-oneclick assets/server_2x.mjs records one
// account answering 200 for zai_glm-5.3-flash and 403 810000 for
// zaicoding_glm-5.3 at the same moment.
func Test810000IsModelScoped(t *testing.T) {
	for _, body := range []string{
		`{"code":"810000","message":"free quota used up"}`,
		`{"code":810000}`,
		`{"error":{"code":"810000"}}`,
		`{"data":{"code":810000,"msg":"额度已用完"}}`,
	} {
		e := classifyHTTPFor(403, []byte(body), "zai_glm-5.3-flash")
		if e.Class != uperr.ClassHardCredit {
			t.Fatalf("%s: class = %s, want %s", body, e.Class, uperr.ClassHardCredit)
		}
		if e.Model != "zai_glm-5.3-flash" {
			t.Fatalf("%s: model = %q, want the route it ran out on", body, e.Model)
		}
		if e.Code != "810000" {
			t.Fatalf("%s: code = %q, want 810000", body, e.Code)
		}
	}
}

// TestBusyCodesAreNotQuotaWalls pins that the throttles (810001/810002) are
// soft rate limits, never allowance walls: treating them as "out of credit"
// would rest an account that is merely being asked to slow down.
func TestBusyCodesAreNotQuotaWalls(t *testing.T) {
	for _, code := range []string{"810001", "810002"} {
		e := classifyHTTPFor(429, []byte(`{"code":"`+code+`"}`), "zai_glm-5.3-flash")
		if e.Class != uperr.ClassSoftRate {
			t.Fatalf("%s: class = %s, want %s", code, e.Class, uperr.ClassSoftRate)
		}
	}
}

// TestBalanceWordingStaysAccountWide pins that a body with no code still rests
// the whole account: a plain balance is spent for every model at once, so the
// model-scoped path must not fire on wording alone.
func TestBalanceWordingStaysAccountWide(t *testing.T) {
	e := classifyHTTPFor(402, []byte(`{"message":"积分不足"}`), "zai_glm-5.3-flash")
	if e.Class != uperr.ClassHardCredit {
		t.Fatalf("class = %s", e.Class)
	}
	if e.Model != "" {
		t.Fatalf("a wording-only balance error got scoped to %q; the account's balance is shared", e.Model)
	}
}
