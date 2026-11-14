package qoder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"bargainproxy/internal/config"
)

// BP-324 (2026-11-14): the quota probe was realm-blind twice over — it hit the
// CN-constant host (d.openAPIBase) and authenticated with the COSY session
// token, so an intl probe carried an intl-issued token to the CN host and
// answered 401 TOKEN_EXPIRE "token is not active" on every hourly run. Fixed:
// the host is rt.openAPIBase() (realm-aware) and intl authenticates with the
// jobToken-exchange bearer (jt-) — measured live 200 with the full document.
// These tests pin the per-realm token choice and the 401→bearer fallback.

func bp324QuotaServer(t *testing.T, quotaStatuses []int) (*httptest.Server, *[]string) {
	t.Helper()
	var auths []string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v3/user/jobToken": // COSY session bootstrap
			fmt.Fprint(w, jobTokenJSON)
		case "/api/v1/jobToken/exchange": // openBearer
			fmt.Fprint(w, `{"token":"jt-bp324","expires_in":3600000}`)
		case quotaPath:
			n := atomic.AddInt32(&calls, 1)
			auths = append(auths, r.Header.Get("Authorization"))
			idx := int(n) - 1
			if idx >= len(quotaStatuses) {
				idx = len(quotaStatuses) - 1
			}
			if quotaStatuses[idx] != http.StatusOK {
				w.WriteHeader(quotaStatuses[idx])
				fmt.Fprint(w, `{"code":"TOKEN_EXPIRE","message":"token is not active"}`)
				return
			}
			fmt.Fprint(w, `{"userQuota":{"remaining":298,"total":300,"used":2,"unit":"credits"},"addOnQuota":{"remaining":500,"total":500}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	return srv, &auths
}

func TestBP324IntlQuotaUsesExchangeBearer(t *testing.T) {
	srv, auths := bp324QuotaServer(t, []int{http.StatusOK})
	defer srv.Close()
	d := New(QoderSettingsForTest(srv.URL), nil)
	rt := &runtime{
		realm:    realmConfig{Name: "intl", OpenAPI: srv.URL, Gateway: srv.URL},
		sessions: newSessionTable(),
	}
	rt.creds = Creds{PAT: "pt-test"}
	doc, err := d.fetchUsageDoc(context.Background(), rt)
	if err != nil {
		t.Fatalf("fetchUsageDoc: %v", err)
	}
	if len(*auths) != 1 {
		t.Fatalf("quota calls = %d, want 1", len(*auths))
	}
	if (*auths)[0] != "Bearer jt-bp324" {
		t.Fatalf("intl quota auth = %q, want the exchange bearer", (*auths)[0])
	}
	if got := toInt64(doc["userQuota"].(map[string]any)["remaining"]); got != 298 {
		t.Fatalf("remaining = %d, want 298", got)
	}
}

func TestBP324CNQuotaFallsBackToBearerOn401(t *testing.T) {
	srv, auths := bp324QuotaServer(t, []int{http.StatusUnauthorized, http.StatusOK})
	defer srv.Close()
	d := New(QoderSettingsForTest(srv.URL), nil)
	rt := &runtime{
		realm:    realmConfig{Name: "cn", OpenAPI: srv.URL, Gateway: srv.URL},
		sessions: newSessionTable(),
	}
	rt.creds = Creds{PAT: "pt-test"}
	doc, err := d.fetchUsageDoc(context.Background(), rt)
	if err != nil {
		t.Fatalf("fetchUsageDoc: %v", err)
	}
	if len(*auths) != 2 {
		t.Fatalf("quota calls = %d, want 2 (session token, then bearer fallback)", len(*auths))
	}
	if (*auths)[0] != "Bearer sec" {
		t.Fatalf("CN first auth = %q, want the session token", (*auths)[0])
	}
	if (*auths)[1] != "Bearer jt-bp324" {
		t.Fatalf("CN fallback auth = %q, want the exchange bearer", (*auths)[1])
	}
	if got := toInt64(doc["userQuota"].(map[string]any)["remaining"]); got != 298 {
		t.Fatalf("remaining = %d, want 298", got)
	}
}

// QoderSettingsForTest steers both bases at the fake server (the CN override
// path of realmCfgFor) and is only used with runtime fixtures that carry their
// own realm config.
func QoderSettingsForTest(url string) config.QoderSettings {
	return config.QoderSettings{Enabled: true, GatewayBase: url, OpenBase: url, MaxInFlight: 1}
}
