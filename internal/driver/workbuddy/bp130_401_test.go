package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-130: a 401 is worth one PROVEN retry — force-refresh (single-flight),
// then resend the SAME request with the new token. Upstream sometimes
// invalidates a still-fresh token; without this every transient 401 cost a
// 10-minute pool rest for nothing (misakano #66, magpie workbuddy 0.1.31).
func Test401SelfHealsWithOneRefresh(t *testing.T) {
	chatHits := 0
	refreshed := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case refreshPath:
			refreshed = true
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"fresh-access","refreshToken":"fresh-refresh","expiresIn":3600}}`)
		case "/v2/chat/completions":
			chatHits++
			if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "fresh-access" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"code":12153,"msg":"token invalid"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
			fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer up.Close()

	d := testDriver(up.URL)
	acc := testAccount()
	creds, _ := ParseCreds(acc.CredsJSON)
	creds.AccessToken = "stale-access"
	creds.RefreshToken = "stale-refresh"
	raw, _ := json.Marshal(creds)
	acc.CredsJSON = raw

	call := wbCall("glm-5.3", "")
	call.Account = acc
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v (a 401 followed by a good refresh must heal in place)", err)
	}
	if resp == nil {
		t.Fatal("no response")
	}
	if !refreshed || chatHits != 2 {
		t.Fatalf("refreshed=%v chatHits=%d, want one refresh then one retry", refreshed, chatHits)
	}
	fresh, _ := ParseCreds(acc.Credentials())
	if fresh.AccessToken != "fresh-access" {
		t.Fatalf("credentials not persisted: %q", fresh.AccessToken)
	}
}

// A 401 whose refresh also fails must still surface as ClassAuth (no infinite
// hammering).
func Test401WithDeadRefreshStaysAuth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":12153,"msg":"session dead"}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	acc := testAccount()
	call := wbCall("glm-5.3", "")
	call.Account = acc
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401 auth error", err)
	}
}
