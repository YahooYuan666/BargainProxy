package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-225 (misakano 847deb10, 2026-10-30 international 5.9.4 captures): the
// global personal chat wears "true" on X-Private-Data (the international
// build ships DefaultModelOptimizationOff=true), the 5.9.4 UA, and carries
// X-Requested-With. BP-170's pinned "false" was a CN capture wearing an
// international hat — every global chat was a standing fingerprint.
func TestBP225GlobalChatFingerprint(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		serveOKSSE(w)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "wb-g", Channel: model.ChannelWorkBuddy, CredsJSON: mustGlobalCredsJSON(t)},
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3", SessionKey: "sk-1", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if v := got.Get("X-Private-Data"); v != "true" {
		t.Fatalf("global X-Private-Data = %q, want true (international build flips the default)", v)
	}
	if v := got.Get("User-Agent"); v != desktopUAGlobal {
		t.Fatalf("global chat UA = %q, want %q", v, desktopUAGlobal)
	}
	if v := got.Get("X-IDE-Version"); v != "5.9.4" {
		t.Fatalf("X-IDE-Version = %q, want 5.9.4", v)
	}
	if v := got.Get("X-Requested-With"); v != "XMLHttpRequest" {
		t.Fatalf("chat X-Requested-With = %q, want XMLHttpRequest", v)
	}
	if v := got.Get("X-Domain"); v != "www.workbuddy.ai" {
		t.Fatalf("X-Domain = %q, want www.workbuddy.ai", v)
	}
}

// The CN personal account keeps BP-170's "false" — the flip is per
// distribution, not a blanket change.
func TestBP225CNChatKeepsPrivateDataFalse(t *testing.T) {
	d := testDriver("http://unused")
	req, _ := http.NewRequest("POST", "https://api.example.cn/v2/chat/completions", nil)
	d.applyClientFingerprint(req, testCreds(), driver.ChatCall{
		Account:       &model.Account{ID: "wb-c", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3"},
	})
	if v := req.Header.Get("X-Private-Data"); v != "false" {
		t.Fatalf("CN X-Private-Data = %q, want false (BP-170 capture set still governs CN)", v)
	}
}

func mustGlobalCredsJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"access_token": "tok", "refresh_token": "r", "uid": "u-g", "realm": "global"})
	if err != nil {
		t.Fatalf("global creds: %v", err)
	}
	return raw
}

// serveOKSSE is the minimal valid chat stream the fingerprint servers answer
// with.
func serveOKSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
	w.Write([]byte("data: [DONE]\n\n"))
}
