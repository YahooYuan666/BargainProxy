package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// TestRefreshDerivesMissingDeviceID is the BP-119 regression test.
//
// Field 2026-10-26: the panel's 刷新额度 on the overseas AutoClaw account
// answered `upstream error class=auth status=401: autoclaw: session dead:
// Please log in to continue.` The account was healthy; its access token had
// simply aged out and every refresh attempt had been refused, because the
// credential blob imported from the desktop client carries no device_id and
// /userapi/v1/refresh answers 400001 "Request data error" for an empty one
// (verified live: "" -> 400001, <uuid> -> 200).
func TestRefreshDerivesMissingDeviceID(t *testing.T) {
	var mu sync.Mutex
	var gotDeviceID string
	var refreshHits int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(r.URL.Path, "/userapi/v1/refresh") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		refreshHits++
		var body struct {
			DeviceID string `json:"device_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDeviceID = body.DeviceID
		if body.DeviceID == "" {
			// The real upstream's answer for an empty device id.
			_, _ = w.Write([]byte(`{"code":400001,"msg":"Request data error","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"access_token":"Bearer new","refresh_token":"rt2"}}`))
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{
		Enabled: true, UserAPIBase: srv.URL, OverseasBase: srv.URL, AppSecret: "test-secret",
	}, nil)
	acc := &model.Account{
		ID:      "autoclaw:182537",
		Channel: model.ChannelAutoClaw,
		Realm:   realmOverseas,
		CredsJSON: (&Creds{
			AccessToken:  "Bearer old",
			RefreshToken: "rt1",
			Region:       realmOverseas,
			// DeviceID deliberately empty: this is what the client import leaves.
		}).Marshal(),
	}
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("Refresh: %v (a missing device_id must be derived, not sent empty)", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshHits == 0 {
		t.Fatal("refresh endpoint was never called")
	}
	if gotDeviceID == "" {
		t.Fatal("refresh sent an EMPTY device_id — upstream answers 400001 and the account goes silently dead")
	}
	// It must be persisted too: the next refresh has to sign with the same id.
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		t.Fatalf("ParseCreds after refresh: %v", err)
	}
	if cr.DeviceID != gotDeviceID {
		t.Fatalf("derived device id not persisted: sent %q, stored %q", gotDeviceID, cr.DeviceID)
	}
	if !strings.Contains(cr.AccessToken, "new") {
		t.Fatalf("rotated token not stored: %q", cr.AccessToken)
	}
}

// TestRefreshUsesOverseasLane: the overseas account's refresh must go to the
// overseas host. The two lanes are separate credential universes, so a
// CN-hosted refresh of a global token is refused — the silent failure that left
// the account dead.
//
// Two DIFFERENT test servers are required: a single shared server cannot tell
// the lanes apart, so a test built on one would stay green with the lane
// hard-wired to the wrong host.
func TestRefreshUsesOverseasLane(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, "cn")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":400001,"msg":"Request data error","data":null}`))
	}))
	defer cnSrv.Close()
	ovSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, "overseas")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"access_token":"Bearer new"}}`))
	}))
	defer ovSrv.Close()

	d := New(config.AutoClawSettings{
		Enabled: true, UserAPIBase: cnSrv.URL, OverseasBase: ovSrv.URL, AppSecret: "s",
	}, nil)
	acc := &model.Account{
		ID: "autoclaw:182537", Channel: model.ChannelAutoClaw, Realm: realmOverseas,
		CredsJSON: (&Creds{
			AccessToken: "Bearer t", RefreshToken: "rt", Region: realmOverseas,
			DeviceID: "11111111-2222-3333-4444-555555555555",
		}).Marshal(),
	}
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 || hits[0] != "overseas" {
		t.Fatalf("refresh lane = %v, want [overseas] (a CN-hosted refresh of a global token is refused)", hits)
	}
}

// TestDeviceIDIsStable pins the derivation: two calls for the same account must
// agree, or every refresh would present a new device to the upstream.
func TestDeviceIDIsStable(t *testing.T) {
	a := &model.Account{ID: "autoclaw:182537"}
	b := &model.Account{ID: "autoclaw:182537"}
	if x, y := deviceIDFor(&Creds{}, a), deviceIDFor(&Creds{}, b); x != y || x == "" {
		t.Fatalf("derivation not stable: %q vs %q", x, y)
	}
	// An id already in the blob wins: never overwrite what the client gave us.
	if got := deviceIDFor(&Creds{DeviceID: "client-given"}, a); got != "client-given" {
		t.Fatalf("stored device id overwritten: %q", got)
	}
}

// TestCheckinUsesOverseasHost is the other half of BP-119.
//
// The daily check-in hard-coded the CN gateway, so the global account's task
// failed every day with `401 {"error":"Invalid token"}` while the CN account
// checked in fine. Verified live 2026-10-26: the same token answers 200 with
// {"success":true,"reward_points":400} on the overseas host.
func TestCheckinUsesOverseasHost(t *testing.T) {
	var cnHits, ovHits int
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnHits++
		_, _ = w.Write([]byte(`{"code":410000,"msg":"Please log in to continue.","data":null}`))
	}))
	defer cnSrv.Close()
	ovSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ovHits++
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_completed":false,"reward_points":400}}`))
	}))
	defer ovSrv.Close()

	d := New(config.AutoClawSettings{
		Enabled: true, UserAPIBase: cnSrv.URL, OverseasBase: ovSrv.URL, AppSecret: "s",
	}, nil)
	acc := &model.Account{
		ID: "autoclaw:182537", Channel: model.ChannelAutoClaw, Realm: realmOverseas,
		CredsJSON: (&Creds{AccessToken: "Bearer t", Region: realmOverseas}).Marshal(),
	}
	checkedIn, reward, enable, err := d.Checkin(context.Background(), acc)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if checkedIn || reward != 400 || !enable {
		t.Fatalf("checkin result = (%v, %d, %v), want (false, 400, true)", checkedIn, reward, enable)
	}
	if cnHits != 0 || ovHits == 0 {
		t.Fatalf("check-in lane hits cn=%d overseas=%d, want cn=0 overseas>0", cnHits, ovHits)
	}
}

// TestCheckinCNAccountStaysOnCN guards the other direction: the fix must not
// push CN accounts onto the overseas host.
func TestCheckinCNAccountStaysOnCN(t *testing.T) {
	var lane string
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lane = "cn"
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_completed":false,"reward_points":200}}`))
	}))
	defer cnSrv.Close()
	ovSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lane = "overseas"
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_completed":false,"reward_points":400}}`))
	}))
	defer ovSrv.Close()

	d := New(config.AutoClawSettings{
		Enabled: true, UserAPIBase: cnSrv.URL, OverseasBase: ovSrv.URL, AppSecret: "s",
	}, nil)
	acc := &model.Account{
		ID: "autoclaw:153f2fb1", Channel: model.ChannelAutoClaw, Realm: "cn",
		CredsJSON: (&Creds{AccessToken: "Bearer t", Region: "cn"}).Marshal(),
	}
	_, reward, _, err := d.Checkin(context.Background(), acc)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if lane != "cn" || reward != 200 {
		t.Fatalf("CN account check-in went to lane %q with reward %d, want cn/200", lane, reward)
	}
}
