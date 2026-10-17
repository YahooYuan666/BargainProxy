// ticket 37: trae token refresh — the driver used to be a stub that never
// refreshed, so imported JWTs silently expired and chat failed 1001.
package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestRefreshRotatesToken ExchangeToken 真刷新：Token/RefreshToken 轮换、
// 毫秒到期归一化为秒、CredsDirty 置位（由刷新任务落盘）。
func TestRefreshRotatesToken(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["RefreshToken"] != "rt-old" || req["ClientID"] != traeOAuthClient {
			t.Errorf("exchange body = %v", req)
		}
		expMs := time.Now().Add(48 * time.Hour).UnixMilli()
		fmt.Fprintf(w, `{"Result":{"Token":"jwt-new","TokenExpireAt":%d,"RefreshToken":"rt-new"}}`, expMs)
	}))
	defer up.Close()
	oldHost := traeOAuthHost
	traeOAuthHost = up.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	d := New(config.TraeSettings{Enabled: true}, nil)
	creds, _ := json.Marshal(map[string]any{
		"access_token": "jwt-old", "refresh_token": "rt-old", "uid": "u1",
		"expires_at": time.Now().Add(-time.Hour).Unix(), // expired → refresh
	})
	acc := &model.Account{ID: "trae:1", Channel: model.ChannelTrae, CredsJSON: creds}
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if hits != 1 {
		t.Fatalf("exchange hits = %d, want 1", hits)
	}
	var got map[string]any
	if err := json.Unmarshal(acc.CredsJSON, &got); err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "jwt-new" || got["refresh_token"] != "rt-new" {
		t.Fatalf("creds after refresh = %v", got)
	}
	exp := int64(0)
	switch v := got["expires_at"].(type) {
	case float64:
		exp = int64(v)
	}
	if exp < time.Now().Add(47*time.Hour).Unix() || exp > time.Now().Add(49*time.Hour).Unix() {
		t.Fatalf("expires_at = %d, want ~48h out (ms normalized to seconds)", exp)
	}
	if !acc.CredsDirty {
		t.Fatal("CredsDirty must be set so the refresh task persists")
	}
}

// TestRefreshSkipsFreshToken 24h 内不过期的 token 不动上游。
func TestRefreshSkipsFreshToken(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer up.Close()
	oldHost := traeOAuthHost
	traeOAuthHost = up.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	d := New(config.TraeSettings{Enabled: true}, nil)
	creds, _ := json.Marshal(map[string]any{
		"access_token": "jwt-old", "refresh_token": "rt-old",
		"expires_at": time.Now().Add(40 * time.Hour).Unix(),
	})
	acc := &model.Account{ID: "trae:1", Channel: model.ChannelTrae, CredsJSON: creds}
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if hits != 0 {
		t.Fatalf("fresh token must not hit upstream: hits=%d", hits)
	}
}

// TestRefreshRejectedKeepsOldCreds 上游拒绝（无 Token）→ ClassAuth 错误且
// creds 不被改写（旧 refresh token 仍可重试）。
func TestRefreshRejectedKeepsOldCreds(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Result":{}}`)
	}))
	defer up.Close()
	oldHost := traeOAuthHost
	traeOAuthHost = up.URL
	t.Cleanup(func() { traeOAuthHost = oldHost })

	d := New(config.TraeSettings{Enabled: true}, nil)
	raw, _ := json.Marshal(map[string]any{
		"access_token": "jwt-old", "refresh_token": "rt-old",
		"expires_at": time.Now().Add(-time.Hour).Unix(),
	})
	acc := &model.Account{ID: "trae:1", Channel: model.ChannelTrae, CredsJSON: raw}
	err := d.Refresh(context.Background(), acc)
	ue, ok := uperr.As(err)
	if err == nil || !ok || ue.Class != uperr.ClassAuth {
		t.Fatalf("err = %v, want auth class", err)
	}
	if string(acc.CredsJSON) != string(raw) {
		t.Fatalf("failed refresh must not rewrite creds")
	}
}
