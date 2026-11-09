package qoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-305：Qoder 国际站接入。双区参考（均 MIT）：
//   - hub REALM_CONFIGS：intl openapi=openapi.qoder.sh、gateway=api3.qoder.sh、
//     UMID env=3、home=.qoder；cn 原样；COSY 公钥全域一套；legacy check-in intl 404。
//   - EchoPing07 Go 版：intl PAT 同一条 exchange；机器头纪律（派生身份不发）。

func TestBP305RealmCfg(t *testing.T) {
	cn := realmCfg("cn")
	if cn.OpenAPI != openBase || cn.Gateway != authBase {
		t.Fatalf("cn hosts drifted: %s %s", cn.OpenAPI, cn.Gateway)
	}
	if cn.UMDir != ".qoder-cn" || cn.UmidEnv != "0" || !strings.Contains(cn.Website, "qoder.com.cn") {
		t.Fatalf("cn realm wrong: %+v", cn)
	}
	in := realmCfg("intl")
	if in.OpenAPI != "https://openapi.qoder.sh" || in.Gateway != "https://api3.qoder.sh" {
		t.Fatalf("intl hosts wrong: %s %s", in.OpenAPI, in.Gateway)
	}
	if in.UMDir != ".qoder" || in.UmidEnv != "3" || in.Website != "https://qoder.com" {
		t.Fatalf("intl realm wrong: %+v", in)
	}
	// 未知值回落 cn：存量账号（realm 空）语义不变。
	if got := normalizeRealm(""); got != "cn" {
		t.Fatalf("empty realm = %q, want cn", got)
	}
	if got := normalizeRealm("nonsense"); got != "cn" {
		t.Fatalf("unknown realm = %q, want cn", got)
	}
	if normalizeRealm("intl") != "intl" {
		t.Fatal("intl must round-trip")
	}
}

// TestBP305GuardRealmModel 跨区模型守卫：只在「实时清单 + key 从未服务」时拒。
func TestBP305GuardRealmModel(t *testing.T) {
	live := &ModelCatalog{
		Caps: map[string]*ModelCaps{"qfmodel": {}},
		Off:  map[string]bool{"dfmodel": true},
		Live: true,
	}
	// 从未服务的 key → ClassNotFound（级联信号）。
	err := guardRealmModel(live, "smodel", realmCfg("cn"))
	if ue, ok := uperr.As(err); err == nil || !ok || ue.Class != uperr.ClassNotFound {
		t.Fatalf("unserved key must be ClassNotFound, got %v", err)
	}
	// 在册 / 下架中 / 空目录 / 静态兜底 → 不拦。
	if err := guardRealmModel(live, "qfmodel", realmCfg("cn")); err != nil {
		t.Fatalf("served key must pass: %v", err)
	}
	if err := guardRealmModel(live, "dfmodel", realmCfg("cn")); err != nil {
		t.Fatalf("off key must ride to the upstream 400 (BP-304 recast): %v", err)
	}
	static := &ModelCatalog{Caps: map[string]*ModelCaps{"qfmodel": {}}}
	if err := guardRealmModel(static, "smodel", realmCfg("intl")); err != nil {
		t.Fatalf("static fallback must never gate: %v", err)
	}
	if err := guardRealmModel(nil, "smodel", realmCfg("intl")); err != nil {
		t.Fatal("nil catalog must never gate")
	}
}

// TestBP305CheckinHeadersMachinePair hub issue #10 纪律：真实身份才发机器对，
// 派生身份一个都不发（伪造 token 会被上游静默过滤每日活动）；Origin 按区。
func TestBP305CheckinHeadersMachinePair(t *testing.T) {
	rc := realmCfg("intl")
	h := checkinHeaders("bt", "uid", "tok", "ty", "runtime-info", rc)
	if h["Cosy-MachineToken"] != "tok" || h["Cosy-MachineType"] != "ty" {
		t.Fatalf("real identity must ride: %+v", h)
	}
	if h["Origin"] != "https://qoder.com" || h["Referer"] != "https://qoder.com/" {
		t.Fatalf("intl origin wrong: %+v", h)
	}
	h2 := checkinHeaders("bt", "uid", "dtok", "dty", "derived", rc)
	if _, has := h2["Cosy-MachineToken"]; has {
		t.Fatal("derived token must NOT ride (hub issue #10: it filters the daily entry)")
	}
	if _, has := h2["Cosy-MachineType"]; has {
		t.Fatal("derived type must NOT ride")
	}
	h3 := checkinHeaders("bt", "uid", "tok", "ty", "runtime-info", realmCfg("cn"))
	if h3["Origin"] != "https://qoder.com.cn" {
		t.Fatalf("cn origin drifted: %s", h3["Origin"])
	}
}

// TestBP305FinderPerRealm helper 按区发现：intl 先找 .qoder CLI 缓存，cn 先找
// .qoder-cn。桌面路径依赖本机安装形态，不进单测（实机已核对两份 helper）。
func TestBP305FinderPerRealm(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	// 真机：两份 CLI 缓存都在（2026-11-10 实测），intl 命中 .qoder 下的 helper。
	got := findQoderRuntimeInfo(realmCfg("intl"))
	if got == "" || !strings.Contains(got, filepath.Join(home, ".qoder", ".bin")) {
		t.Fatalf("intl finder = %q, want the .qoder CLI cache helper", got)
	}
	gotCN := findQoderRuntimeInfo(realmCfg("cn"))
	if gotCN == "" || !strings.Contains(gotCN, filepath.Join(home, ".qoder-cn", ".bin")) {
		t.Fatalf("cn finder = %q, want the .qoder-cn CLI cache helper", gotCN)
	}
	if got == gotCN {
		t.Fatal("the two realms must read DIFFERENT helpers")
	}
}

// TestBP305RuntimeRealm runtime 携带区域配置；cfg 覆盖只作用于 cn。
func TestBP305RuntimeRealm(t *testing.T) {
	d := New(config.QoderSettings{Enabled: true}, nil)
	acc := &model.Account{ID: "q-i", Channel: model.ChannelQoder, Realm: "intl",
		CredsJSON: []byte(`{"pat":"pt-x","machine_id":"a","machine_token":"b","machine_type":"c"}`)}
	rt, err := d.runtimeFor(acc)
	if err != nil {
		t.Fatal(err)
	}
	if rt.realm.Name != "intl" || rt.realm.Gateway != "https://api3.qoder.sh" {
		t.Fatalf("intl runtime realm = %+v", rt.realm)
	}
	// cn + cfg 覆盖：既有行为保持。
	d.cfg.OpenBase = "https://open.example.cn"
	acc2 := &model.Account{ID: "q-c", Channel: model.ChannelQoder,
		CredsJSON: []byte(`{"pat":"pt-y","machine_id":"a","machine_token":"b","machine_type":"c"}`)}
	rt2, err := d.runtimeFor(acc2)
	if err != nil {
		t.Fatal(err)
	}
	if rt2.realm.OpenAPI != "https://open.example.cn" {
		t.Fatalf("cn cfg override lost: %s", rt2.realm.OpenAPI)
	}
	// intl 不吃 cn 的覆盖。
	acc3 := &model.Account{ID: "q-i2", Channel: model.ChannelQoder, Realm: "intl",
		CredsJSON: []byte(`{"pat":"pt-z","machine_id":"a","machine_token":"b","machine_type":"c"}`)}
	rt3, err := d.runtimeFor(acc3)
	if err != nil {
		t.Fatal(err)
	}
	if rt3.realm.OpenAPI != "https://openapi.qoder.sh" {
		t.Fatalf("intl must ignore cn overrides: %s", rt3.realm.OpenAPI)
	}
	_ = context.Background()
	_ = config.Default
	_ = ir.ChatRequest{}
}

// TestBP305RealIdentityRidesOnCampaigns 正向钉死：helper 读到真实身份时，
// campaigns 请求必须带上 Cosy-MachineToken/Type（BP-237 的原始修复不能被
// hub issue #10 纪律误伤）。
func TestBP305RealIdentityRidesOnCampaigns(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "runtime-info.bat")
	body := "@echo off\r\nset /p ACCOUNT=\r\n" +
		`echo {"machineToken":"REALTOK","machineType":"REALTYP"}` + "\r\n"
	if !isWindows() {
		exe = filepath.Join(dir, "runtime-info")
		body = "#!/bin/sh\nread -r ACCOUNT\n" +
			`echo '{"machineToken":"REALTOK","machineType":"REALTYP"}'` + "\n"
	}
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newCampaignsServer(t, `{"campaigns":[{"id":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":100}}]}`)
	cfg := config.QoderSettings{GatewayBase: srv.URL, OpenBase: srv.URL, CheckinRuntimeInfo: exe}
	d := New(cfg, nil)
	// 带真实 UID：helper 以 uid 取身份，空 uid 直接走 derived（BP-237 语义）。
	acc := testQoderAccountWithUID(t)
	if _, _, _, err := d.Checkin(context.Background(), acc); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if n := len(srv.tokens); n == 0 {
		t.Fatal("campaigns list was never requested")
	} else if srv.tokens[n-1] != "REALTOK" || srv.types[n-1] != "REALTYP" {
		t.Fatalf("real identity must ride, sent (%q, %q)", srv.tokens[n-1], srv.types[n-1])
	}
}
