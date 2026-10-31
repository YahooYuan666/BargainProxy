package workbuddy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/protocol/openai"
)

// BP-158/BP-197/BP-225: the fingerprint must match the CURRENT desktop
// builds — CN 5.9.3 with CLI 2.165.0; the international feed moved to 5.9.4
// (misakano 847deb10, 2026-10-30: package unpacked, /v2/update reports
// version=5.9.4) with the same CLI 2.165.0.
func TestBP158Fingerprints(t *testing.T) {
	if !strings.Contains(desktopUA, "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0") {
		t.Fatalf("CN UA = %q, want the 5.9.3 / CLI 2.165.0 build", desktopUA)
	}
	if !strings.Contains(desktopUAGlobal, "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0") {
		t.Fatalf("global UA = %q, want the 5.9.4 build (BP-225, misakano 847deb10)", desktopUAGlobal)
	}
	// BP-225: the REST family wears a different UA on both realms.
	if desktopUARestCN != "WorkBuddy/5.9.3 CLI/2.165.0" {
		t.Fatalf("CN REST UA = %q, want the two-segment form", desktopUARestCN)
	}
	if desktopUARestGlobal != "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0" {
		t.Fatalf("global REST UA = %q, want the slug-first form", desktopUARestGlobal)
	}
}

// BP-158/BP-197: X-Agent-Runtime is a 5.9 header. It was CN-only on the
// 5.8.4 international build (BP-158); the international feed has since
// moved to the same 5.9.3 build and sends it too, so BOTH realms must carry
// it now.
func TestBP158AgentRuntimeCNOnly(t *testing.T) {
	d := testDriver("http://unused")
	orig := d.cfg.WebBaseCN
	defer func() { d.cfg.WebBaseCN = orig }()

	mkReq := func() *http.Request {
		r, _ := http.NewRequest("POST", "https://api.example.cn/v2/chat/completions", nil)
		return r
	}
	call := driver.ChatCall{
		Account:       &model.Account{ID: "wb-rt", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3"},
	}

	cnReq := mkReq()
	d.applyClientFingerprint(cnReq, testCreds(), call)
	if cnReq.Header.Get("X-Agent-Runtime") != "local-agent" {
		t.Fatalf("CN X-Agent-Runtime = %q, want local-agent", cnReq.Header.Get("X-Agent-Runtime"))
	}
	if cnReq.Header.Get("X-Stainless-Timeout") != "1200" {
		t.Fatalf("CN X-Stainless-Timeout = %q, want 1200", cnReq.Header.Get("X-Stainless-Timeout"))
	}

	greq := mkReq()
	d.applyClientFingerprint(greq, mustGlobalCreds(t), call)
	if greq.Header.Get("X-Agent-Runtime") != "local-agent" {
		t.Fatalf("global X-Agent-Runtime = %q, want local-agent (BP-197: the 5.9.3 international build sends it)", greq.Header.Get("X-Agent-Runtime"))
	}
	if greq.Header.Get("X-Stainless-Timeout") != "1200" {
		t.Fatalf("global X-Stainless-Timeout = %q, want 1200", greq.Header.Get("X-Stainless-Timeout"))
	}
}

// BP-159: an assistant turn's reasoning_content must reach WorkBuddy as the
// `reasoning` field — the host silently drops reasoning_content, and folding
// it into content as <think> is not what the official client does.
func TestBP159ReasoningRidesTheReasoningField(t *testing.T) {
	wire := openai.WireMessage{
		Role:             "assistant",
		ReasoningContent: "I should call the file tool first.",
	}
	// Ingress normalization.
	irMsg := ir.Message{Role: ir.RoleAssistant, Reasoning: wire.ReasoningText()}
	if irMsg.Reasoning == "" {
		t.Fatal("ingress dropped reasoning_content")
	}

	d := testDriver("http://unused")
	call := driver.ChatCall{
		Account:       &model.Account{ID: "wb-r", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
				irMsg,
			},
		},
	}
	body, err := d.buildBody(call, testCreds(), false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body json: %v", err)
	}
	var found bool
	for _, m := range doc.Messages {
		if m["role"] != "assistant" {
			continue
		}
		if m["reasoning"] != "I should call the file tool first." {
			t.Fatalf("assistant reasoning field = %v", m["reasoning"])
		}
		if _, ok := m["reasoning_content"]; ok {
			t.Fatal("reasoning_content must not be sent — the host drops it")
		}
		found = true
	}
	if !found {
		t.Fatal("assistant message missing from the wire")
	}
}

// reasoning precedence: reasoning_content wins over reasoning; the details
// array is the fallback shape.
func TestBP159ReasoningPrecedence(t *testing.T) {
	m := openai.WireMessage{
		Role:             "assistant",
		ReasoningContent: "first wins",
		Reasoning:        "second",
	}
	if got := m.ReasoningText(); got != "first wins" {
		t.Fatalf("precedence broken: %q", got)
	}
	details := openai.WireMessage{
		Role:             "assistant",
		ReasoningDetails: json.RawMessage(`[{"type":"text","text":"from details"}]`),
	}
	if got := details.ReasoningText(); got != "from details" {
		t.Fatalf("details text = %q", got)
	}
}

// mustGlobalCreds builds a global-realm credential set for fingerprint tests.
func mustGlobalCreds(t *testing.T) *Creds {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"access_token": "tok", "refresh_token": "r", "realm": "global"})
	c, err := ParseCreds(raw)
	if err != nil {
		t.Fatalf("global creds: %v", err)
	}
	return c
}
