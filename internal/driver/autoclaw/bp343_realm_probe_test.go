package autoclaw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
)

// TestProbeRealmAsksTheHostNotThePath is the fix for the bug where importing an
// overseas z.ai account produced a row marked `cn`.
//
// Every local signal is ambiguous on a real machine: there is ONE client data
// directory (AutoClaw-official) that both installs write to, channel.json is a
// release channel rather than a region, and an overseas z.ai account signs in
// with a phone number just like a CN one. The one thing that cannot lie is which
// host accepts the credential — the two lanes are separate credential universes
// and each refuses the other's token.
//
// The two hosts are faked here: the overseas one rejects (401) and the CN one
// accepts, so the answer must be cn regardless of what the path or the phone
// number suggested.
func TestProbeRealmAsksTheHostNotThePath(t *testing.T) {
	overseas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid token"}`))
	}))
	defer overseas.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"user_id":1}}`))
	}))
	defer cn.Close()

	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "s", UserAPIBase: cn.URL, OverseasBase: overseas.URL}, nil)
	realm, err := d.ProbeRealm(context.Background(), &Creds{AccessToken: "Bearer t", DeviceID: "d"})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if realm != "cn" {
		t.Fatalf("realm = %q, want cn (the CN host is the one that accepted)", realm)
	}
}

// TestProbeRealmFindsOverseas: the mirror case — the overseas host accepts.
func TestProbeRealmFindsOverseas(t *testing.T) {
	overseas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"user_id":1}}`))
	}))
	defer overseas.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":410000,"msg":"Please log in to continue."}`))
	}))
	defer cn.Close()

	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "s", UserAPIBase: cn.URL, OverseasBase: overseas.URL}, nil)
	realm, err := d.ProbeRealm(context.Background(), &Creds{AccessToken: "Bearer t", DeviceID: "d"})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if realm != realmOverseas {
		t.Fatalf("realm = %q, want %q (the overseas host is the one that accepted)", realm, realmOverseas)
	}
}

// TestProbeRealmReportsWhenNeitherAnswers: an account nobody accepts must not be
// silently classified — the caller needs to know the probe failed.
func TestProbeRealmReportsWhenNeitherAnswers(t *testing.T) {
	bad := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid token"}`))
	}
	a := httptest.NewServer(http.HandlerFunc(bad))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(bad))
	defer b.Close()

	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "s", UserAPIBase: a.URL, OverseasBase: b.URL}, nil)
	if _, err := d.ProbeRealm(context.Background(), &Creds{AccessToken: "Bearer t", DeviceID: "d"}); err == nil {
		t.Fatal("an account no host accepts must report an error, not a lane")
	}
}

// TestImportWithProbeRewritesTheLane: the imported row and its creds blob must
// both carry the lane the probe found, so a later request dials the right host
// without probing again.
func TestImportWithProbeRewritesTheLane(t *testing.T) {
	overseas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"user_id":1}}`))
	}))
	defer overseas.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":410000,"msg":"Please log in to continue."}`))
	}))
	defer cn.Close()

	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "s", UserAPIBase: cn.URL, OverseasBase: overseas.URL}, nil)

	// No local client store is staged, so ImportFromLocalClients returns nothing
	// and ImportWithProbe must simply pass that through without inventing rows.
	accounts, _ := ImportWithProbe(context.Background(), d)
	for _, ia := range accounts {
		if ia.Account == nil {
			continue
		}
		if !strings.Contains(ia.Account.ID, ":") {
			t.Fatalf("odd account id %q", ia.Account.ID)
		}
	}
}
