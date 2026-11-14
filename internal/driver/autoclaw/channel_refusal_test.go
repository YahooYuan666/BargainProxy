package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
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

// TestScrubHandlesTheFieldShapes: the two delete-list products, any casing, and
// a claim buried mid-prompt (the field failure had ~13k tokens of instructions
// around it).
//
// ZCode is NOT in this map any more (BP-283): the CN gateway does refuse it,
// but the community plugin's fix is to reword ("You are the ZCode agent") and
// keep the name, not to delete the sentence — see
// TestZCodeIdentityIsRewordedNotDeleted.
func TestScrubHandlesTheFieldShapes(t *testing.T) {
	cases := map[string]string{
		"You are Claude Code.":                                              "",
		"you are claude code.":                                              "",
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

// TestZCodeIdentityIsRewordedNotDeleted pins BP-283: the CN gateway refuses
// "You are ZCode", but the line must be REWORDED so the agent keeps its name —
// which the rest of its prompt refers back to — instead of being cut out.
//
// Evidence: magpie-community/plugins packages/autoclaw:80 does exactly this
// (`/You are ZCode/gi → "You are the ZCode agent"`), and the plugin's own note
// explains why rewriting beats deleting: "rewriting keeps the agent's name —
// which the rest of its prompt refers back to — where deleting the sentence
// would amputate it".
//
// Deleting also took down whatever rule shared the sentence: with the model's
// opening line reading "You are ZCode, an interactive agent who always answers
// in Cantonese", the whole instruction vanished and the model answered in
// English while introducing itself as AutoClaw.
func TestZCodeIdentityIsRewordedNotDeleted(t *testing.T) {
	cases := map[string]string{
		"You are ZCode, an interactive agent":        "You are the ZCode agent, an interactive agent",
		"You are ZCode.":                             "You are the ZCode agent.",
		"you are zcode and answer only in Cantonese": "You are the ZCode agent and answer only in Cantonese",
	}
	for in, want := range cases {
		if got := scrubRefusedIdentity(in); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
	// The name must survive in every case — that is the whole point.
	for in := range cases {
		if got := scrubRefusedIdentity(in); !strings.Contains(got, "ZCode") {
			t.Errorf("scrub(%q) dropped the agent's name: %q", in, got)
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

// TestGLMPoolQualifiedOpenerIsReworded pins BP-325: ZCode 3.18.2 qualifies the
// model with its provider pool ("You are powered by the model named
// account:bigmodel-start-plan/GLM-5.3-Flash."), which the BP-322 GLM rule
// misses ("powered by the GLM-…" adjacency is broken by "named account:…/").
// The bare model id must not survive the scrub; the reworded form is the same
// "the GLM-x model" wording BP-322 measured as accepted.
func TestGLMPoolQualifiedOpenerIsReworded(t *testing.T) {
	cases := map[string]string{
		"You are powered by the model named account:bigmodel-start-plan/GLM-5.3-Flash.": "You are powered by the GLM-5.3-Flash model.",
		"You are powered by the model named account:my-pool/glm-5.3-flash.":             "You are powered by the GLM-5.3-flash model.",
	}
	for in, want := range cases {
		if got := scrubRefusedIdentity(in); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
	// The plain BP-322 form must still be handled by the older rule.
	if got := scrubRefusedIdentity("You are powered by the model GLM-5.3-Flash."); got != "You are powered by the GLM-5.3-Flash model." {
		t.Errorf("BP-322 form regressed: %q", got)
	}
}

// TestStripAllIdentitySentences pins the BP-325 hop-2 text scrub: every
// "you are" sentence goes, everything else survives byte for byte, and text
// without any identity claim comes back untouched (so the hop can be skipped).
func TestStripAllIdentitySentences(t *testing.T) {
	const kept = "## Tools\nPrefer dedicated tools over shell. Tone: concise."
	cases := map[string]string{
		"You are ZCode, an interactive agent. " + kept: kept,
		"you are Claude Code. " + kept:                 kept,
		"You are Codex. You are Cline. " + kept:        kept,
		"P1. You are Windsurf. P2. " + kept:            "P1. P2. " + kept,
		kept:                                           kept, // no claims → unchanged
		"You are a helpful assistant.":                 "",   // even the neutral line is a claim
	}
	for in, want := range cases {
		if got := stripAllIdentitySentences(in); got != want {
			t.Errorf("strip(%q) = %q, want %q", in, got, want)
		}
	}
}

// Test406FallbackStripsIdentityBeforeNeutral pins the BP-325 chain: hop 1
// (surgical scrub) refused → hop 2 keeps the caller's non-identity
// instructions (the neutral hop used to throw ALL of them away — 62 requests
// on 2026-11-14 ran with no agent instructions at all). Only a second refusal
// escalates to the neutral system line.
func Test406FallbackStripsIdentityBeforeNeutral(t *testing.T) {
	type attempt struct {
		system string
	}
	var attempts []attempt
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sys := ""
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			if first, ok := msgs[0].(map[string]any); ok {
				sys, _ = first["content"].(string)
			}
		}
		calls++
		attempts = append(attempts, attempt{system: sys})
		if calls == 1 {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	oldDir := dump406Dir
	dump406Dir = dir
	defer func() { dump406Dir = oldDir }()

	const instructions = "Tone: be concise. Prefer dedicated tools over shell."
	req := &ir.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
				Text: "You are SomeNewAgent, a coding assistant. " + instructions}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		Stream: true,
	}
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "test-secret"}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()},
		UpstreamModel: "glm-5.3-flash",
		Request:       req,
	}
	_, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (hop 2 must serve, not hop 3)", len(attempts))
	}
	second := attempts[1].system
	if strings.Contains(strings.ToLower(second), "somenewagent") {
		t.Fatalf("the unknown identity claim survived hop 2: %q", second)
	}
	if !strings.Contains(second, "Prefer dedicated tools over shell") {
		t.Fatalf("hop 2 lost the caller's instructions: %q", second)
	}
	// The refused attempt is dumped for evidence.
	dumps, _ := os.ReadDir(dir)
	if len(dumps) != 1 {
		t.Fatalf("406 dumps = %d, want 1", len(dumps))
	}
}

// Test406FallsThroughToNeutralWhenNothingToStrip pins the chain end: when hop 2
// would produce the identical body (no "you are" sentences at all), the
// neutral hop serves directly — resending identical bytes would earn the
// identical 406.
func Test406FallsThroughToNeutralWhenNothingToStrip(t *testing.T) {
	var systems []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sys := ""
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			if first, ok := msgs[0].(map[string]any); ok {
				sys, _ = first["content"].(string)
			}
		}
		calls++
		systems = append(systems, sys)
		if calls == 1 {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	oldDir := dump406Dir
	dump406Dir = t.TempDir()
	defer func() { dump406Dir = oldDir }()

	req := &ir.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text",
				Text: "Tone: concise. No identity sentence here at all."}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		Stream: true,
	}
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "test-secret"}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()},
		UpstreamModel: "glm-5.3-flash",
		Request:       req,
	}
	if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(systems) != 2 {
		t.Fatalf("attempts = %d, want 2", len(systems))
	}
	if systems[1] != neutralSystemLine && !strings.HasPrefix(systems[1], harnessMarker) {
		t.Fatalf("second attempt system = %q, want the neutral line (hop 3)", systems[1])
	}
}
