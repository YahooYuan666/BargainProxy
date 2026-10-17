// ticket 37: chat auth self-heal — a rejected JWT triggers one forced
// ExchangeToken rotation and a single retry.
package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// TestStreamSelfHealsRejectedJWT 首次聊天 1001 → 强制刷新 → 重试成功；
// 两次聊天请求携带的 JWT 必须不同（第二次是刷新后的新 token）。
func TestStreamSelfHealsRejectedJWT(t *testing.T) {
	chatHits := 0
	var tokens []string
	chatUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHits++
		tokens = append(tokens, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		if chatHits == 1 {
			// Stale/rotated JWT: upstream rejects with HTTP 400 + code 1001.
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":1001,"message":"We're sorry, but we are not able to authenticate you."}`)
			return
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(chatUp.Close)

	oauthHits := 0
	oauthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthHits++
		expMs := time.Now().Add(48 * time.Hour).UnixMilli()
		fmt.Fprintf(w, `{"Result":{"Token":"jwt-fresh","TokenExpireAt":%d,"RefreshToken":"rt-2"}}`, expMs)
	}))
	t.Cleanup(oauthUp.Close)
	oldHost := traeOAuthHost
	traeOAuthHost = oauthUp.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	d := testDriver(chatUp.URL, chatUp.URL)
	acc := testAccount()
	// Add a refresh token so the self-heal can rotate.
	var creds map[string]any
	_ = json.Unmarshal(acc.CredsJSON, &creds)
	creds["refresh_token"] = "rt-1"
	acc.CredsJSON, _ = json.Marshal(creds)

	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account: acc, UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("self-heal should recover: %v", err)
	}
	if resp.Message.Content[0].Text != "ok" {
		t.Fatalf("content = %q", resp.Message.Content[0].Text)
	}
	if chatHits != 2 || oauthHits != 1 {
		t.Fatalf("chatHits=%d oauthHits=%d, want 2/1", chatHits, oauthHits)
	}
	if len(tokens) != 2 || tokens[0] == tokens[1] || !strings.Contains(tokens[1], "jwt-fresh") {
		t.Fatalf("retry must use the refreshed JWT: %v", tokens)
	}
	if !acc.CredsDirty {
		t.Fatal("refreshed creds must be marked dirty for persistence")
	}
}

// TestStreamNoHealOnOtherErrors 非 auth 错误不做刷新重试（不放大上游调用）。
func TestStreamNoHealOnOtherErrors(t *testing.T) {
	chatHits := 0
	chatUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHits++
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":1005,"message":"plan entitlement exhausted"}`)
	}))
	t.Cleanup(chatUp.Close)
	oauthHits := 0
	oauthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthHits++
	}))
	t.Cleanup(oauthUp.Close)
	oldHost := traeOAuthHost
	traeOAuthHost = oauthUp.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	d := testDriver(chatUp.URL, chatUp.URL)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account: testAccount(), UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("want entitlement error")
	}
	if chatHits != 1 || oauthHits != 0 {
		t.Fatalf("non-auth errors must not retry: chatHits=%d oauthHits=%d", chatHits, oauthHits)
	}
}
