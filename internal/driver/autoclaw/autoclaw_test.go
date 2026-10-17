package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

func testCreds() []byte {
	return []byte(`{"access_token":"tok","refresh_token":"ref","expires_at":9999999999,"uid":"u1","device_id":"dev","region":"cn"}`)
}

func TestParseCredsAcceptsReferenceCamelCase(t *testing.T) {
	raw := []byte(`{"accessToken":"Bearer tok","refreshToken":"ref","expiresAt":123,"userId":"u9","userName":"n","phone":"1","deviceId":"d","region":"cn","sandboxId":"sb","sandboxEndpoint":"wss://x/autoclaw-cloud/proxy/sb","endTimestamp":0}`)
	c, err := ParseCreds(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.UID != "u9" || c.SandboxID != "sb" || c.Region != "cn" {
		t.Fatalf("unexpected creds: %+v", c)
	}
	if c.bearer() != "Bearer tok" {
		t.Fatalf("bearer = %q", c.bearer())
	}
}

func TestStreamProxySSE(t *testing.T) {
	var gotModel, gotReqModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxyChatPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotReqModel = r.Header.Get("X-Request-Model")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "test-secret"}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()},
		UpstreamModel: "glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	var text strings.Builder
	resp, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if gotModel != "glm-5.3-flash" || gotReqModel != "glm-5.3-flash" {
		t.Fatalf("model=%q reqModel=%q", gotModel, gotReqModel)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestSendCodeAndLogin(t *testing.T) {
	var loginBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case sendCodePath:
			fmt.Fprint(w, `{"code":0,"msg":"ok"}`)
		case loginPath:
			_ = json.NewDecoder(r.Body).Decode(&loginBody)
			fmt.Fprint(w, `{"code":0,"data":{"access_token":"tok2","refresh_token":"r2","user_id":"u2","user_name":"nick"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "test-secret"}, nil)
	if err := d.SendCode(context.Background(), "13800000000"); err != nil {
		t.Fatalf("send code: %v", err)
	}
	acc, err := d.Login(context.Background(), "13800000000", "123456", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if acc.ID != "autoclaw:u2" || acc.Nickname != "nick" {
		t.Fatalf("account = %+v", acc)
	}
	if code, _ := loginBody["code"].(float64); code != 123456 {
		t.Fatalf("login code must be numeric: %v", loginBody["code"])
	}
	if _, err := ParseCreds(acc.CredsJSON); err != nil {
		t.Fatalf("creds: %v", err)
	}
}

func TestCheckinIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != taskCompletePath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"code":0,"data":{"already_completed":true,"reward_points":400}}`)
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "test-secret"}, nil)
	checked, reward, enable, err := d.Checkin(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !checked || !enable || reward != 400 {
		t.Fatalf("checked=%v enable=%v reward=%d", checked, enable, reward)
	}
}

func TestClassifyHardCreditAndAuth(t *testing.T) {
	if ue := classifyHTTP(402, []byte(`{"msg":"insufficient credit"}`)); ue.Class != "hard_credit" {
		t.Fatalf("402 class = %s", ue.Class)
	}
	if ue := classifyBusiness(410000, "session dead"); ue.Class != "auth" {
		t.Fatalf("410000 class = %s", ue.Class)
	}
	if ue := classifyHTTP(429, []byte(`too many`)); ue.Class != "soft_rate" {
		t.Fatalf("429 class = %s", ue.Class)
	}
}

// 源码不再随附 app_secret：未配置时必须给出明确的配置错误，
// 且不能把请求发到上游（否则会拿错误签名触发风控）。
func TestUserapiCallRequiresAppSecret(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		fmt.Fprint(w, `{"code":0}`)
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL}, nil)
	err := d.SendCode(context.Background(), "13800000000")
	if err == nil {
		t.Fatal("SendCode must fail when app_secret is missing")
	}
	if !strings.Contains(err.Error(), "app_secret") {
		t.Fatalf("error must name app_secret, got %v", err)
	}
	if hit {
		t.Fatal("request must not reach upstream without a signing secret")
	}
}
