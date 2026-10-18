package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBP76RefreshCoalescesSameUIDAndSharesRotatedTokens(t *testing.T) {
	for _, samePointer := range []bool{true, false} {
		name := "separate-copies"
		if samePointer {
			name = "same-live-account"
		}
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			arrived := make(chan struct{}, 2)
			release := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				if r.Header.Get("X-Refresh-Token") != "test-refresh-value" {
					t.Errorf("unexpected simulated token")
				}
				arrived <- struct{}{}
				<-release
				if n == 1 {
					fmt.Fprint(w, `{"code":0,"data":{"accessToken":"audit-new-access","refreshToken":"audit-new-refresh","expiresIn":3600}}`)
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"code":12153,"msg":"simulated refresh token replay"}`)
			}))
			defer up.Close()
			d := testDriver(up.URL)
			a := accountWithRefreshToken()
			b := accountWithRefreshToken()
			if samePointer {
				b = a
			}
			results := make(chan error, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go func() { results <- d.RefreshNow(ctx, a) }()
			select {
			case <-arrived:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("first request did not arrive")
			}
			go func() { results <- d.RefreshNow(ctx, b) }()
			select {
			case <-arrived:
			case <-time.After(150 * time.Millisecond):
			}
			close(release)
			errA, errB := <-results, <-results
			credsA, _ := ParseCreds(a.CredsJSON)
			credsB, _ := ParseCreds(b.CredsJSON)
			if hits.Load() != 1 || errA != nil || errB != nil || credsA.RefreshToken != "audit-new-refresh" || credsB.RefreshToken != "audit-new-refresh" {
				t.Fatalf("same UID refresh was not coalesced/shared: HTTP calls=%d errors=[%v, %v] callers observe rotated token=%v; want one request and both callers succeed", hits.Load(), errA, errB, credsA.RefreshToken == "audit-new-refresh" && credsB.RefreshToken == "audit-new-refresh")
			}
		})
	}
}
