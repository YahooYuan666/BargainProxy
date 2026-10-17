package qoder

import (
	"context"
	"encoding/base64"
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

func TestGatewayEncodeRoundtrip(t *testing.T) {
	plain := []byte(`{"hello":"world","n":1}`)
	enc, err := gatewayEncode(plain)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	std := make([]byte, len(enc))
	for i, ch := range []byte(enc) {
		if !c2sOK[ch] {
			t.Fatalf("char out of alphabet: %q", ch)
		}
		std[i] = c2s[ch]
	}
	n := len(std)
	a := n / 3
	orig := make([]byte, n)
	copy(orig[:a], std[n-a:])
	copy(orig[a:n-a], std[a:n-a])
	copy(orig[n-a:], std[:a])
	decoded, err := base64.StdEncoding.DecodeString(string(orig))
	if err != nil {
		t.Fatalf("std decode: %v", err)
	}
	if string(decoded) != string(plain) {
		t.Fatalf("roundtrip = %q", decoded)
	}
}

func TestCheckinFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathJobExchange:
			fmt.Fprint(w, `{"token":"t","expires_in":3600000}`)
		case pathCheckStatus:
			fmt.Fprint(w, `{"status":"CLAIMABLE","currentStreakDays":3}`)
		case pathCheckClaim:
			fmt.Fprint(w, `{"success":true,"rewardCredits":100}`)
		case pathProElig:
			fmt.Fprint(w, `{"eligible":false}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	d := New(config.QoderSettings{Enabled: true, GatewayBase: srv.URL, OpenBase: srv.URL, MaxInFlight: 1}, nil)
	acc := &model.Account{ID: "qoder:u1", CredsJSON: []byte(`{"pat":"pt-test","machine_id":"m","machine_token":"mt","machine_type":"mtype"}`)}
	checked, credits, enable, err := d.Checkin(context.Background(), acc)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if checked || !enable || credits != 100 {
		t.Fatalf("checked=%v enable=%v credits=%d", checked, enable, credits)
	}
}

const jobTokenJSON = `{"id":"u1","name":"nick","userType":"personal_standard","securityOauthToken":"sec","refreshToken":"ref","expireTime":9999999999999}`

func TestStreamFakeGateway(t *testing.T) {
	var gotModelKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v3/user/jobToken":
			fmt.Fprint(w, jobTokenJSON)
		case "/algo/api/v2/model/list":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"fallback"}`)
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			gotModelKey = r.Header.Get("x-model-key")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{Enabled: true, GatewayBase: srv.URL, OpenBase: srv.URL, MaxInFlight: 1}, nil)
	acc := &model.Account{ID: "qoder:u1", CredsJSON: []byte(`{"pat":"pt-test","machine_id":"m","machine_token":"mt","machine_type":"mtype"}`)}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "GLM-5.3",
		Request: &ir.ChatRequest{
			Model:    "GLM-5.3",
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
	if text.String() != "ok" || resp.StopReason != ir.StopEnd {
		t.Fatalf("text=%q stop=%s", text.String(), resp.StopReason)
	}
	if gotModelKey != "gmodel" {
		t.Fatalf("x-model-key = %q, want gmodel (static catalogue fallback)", gotModelKey)
	}
}

func TestLoginPAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/algo/api/v3/user/jobToken" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, jobTokenJSON)
	}))
	defer srv.Close()
	d := New(config.QoderSettings{Enabled: true, GatewayBase: srv.URL, OpenBase: srv.URL, MaxInFlight: 1}, nil)
	acc, err := d.LoginPAT(context.Background(), "pt-abc", "备注")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if acc.ID != "qoder:u1" || acc.Nickname != "备注" {
		raw, _ := json.Marshal(acc)
		t.Fatalf("account = %s", raw)
	}
	if _, err := ParseCreds(acc.CredsJSON); err != nil {
		t.Fatalf("creds: %v", err)
	}
	if _, err := d.LoginPAT(context.Background(), "bad", ""); err == nil {
		t.Fatal("non-pt PAT must be rejected")
	}
}
