package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BP-132: a pasted redirect URL completes the pending Trae flow through the
// exact callback machinery — the fallback for remote deployments where the
// loopback redirect cannot reach this host.
func TestTraePasteWithoutPendingFlow(t *testing.T) {
	p := &Panel{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/trae/paste", strings.NewReader(`{"url":"http://127.0.0.1:1/authorize?code=x"}`))
	p.handleTraePaste(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("no pending flow = %d, want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "没有进行中的 Trae 登录流程") {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
