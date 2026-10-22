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

// TestSystemAsUserMovesThePrompt pins BP-99: the retry carries the caller's own
// system text inside <instructions> in the first user turn, with a neutral
// system line first (the gateway refuses a request whose first message is not
// a system message).
func TestSystemAsUserMovesThePrompt(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "You are Claude Code."}}},
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
	if len(doc.Messages) < 2 {
		t.Fatalf("got %d messages", len(doc.Messages))
	}
	if doc.Messages[0].Role != "system" {
		t.Fatalf("first role = %q, want system", doc.Messages[0].Role)
	}
	if s, _ := doc.Messages[0].Content.(string); s != instructionsLead {
		t.Fatalf("lead = %q, want the neutral default", s)
	}
	user, _ := doc.Messages[1].Content.(string)
	if user == "" {
		t.Fatalf("user turn content is not a string: %v", doc.Messages[1].Content)
	}
	// Field A/B 2026-10-23 (scripts/acprobe2, long conversations, 3 runs each):
	// the gateway refuses the text in a system message in every shape, and
	// accepts it as plain user prose. An <instructions> wrapper was refused too,
	// so the text must go in unwrapped.
	if want := "You are Claude Code.\n\nhello"; user != want {
		t.Fatalf("user turn = %q, want %q", user, want)
	}
	if strings.Contains(user, "<instructions>") {
		t.Fatalf("user turn must not be wrapped: %q", user)
	}
	// The caller's IR must be untouched: a later attempt (another account, the
	// relay path) still needs the original system message.
	if len(req.Messages) != 2 || req.Messages[0].Role != ir.RoleSystem {
		t.Fatalf("the original request was mutated: %+v", req.Messages)
	}
}

// TestSystemAsUserKeepsLateSystemMessages pins that a system message further
// down becomes a user message in place rather than being dropped.
func TestSystemAsUserKeepsLateSystemMessages(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "later rule"}}},
		},
	}
	raw, err := buildBodySystemAsUser(driver.ChatCall{UpstreamModel: "m", Request: req})
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
	found := false
	for _, m := range doc.Messages {
		if m.Role == "user" {
			if s, _ := m.Content.(string); s == "later rule" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the late system message was dropped: %+v", doc.Messages)
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
