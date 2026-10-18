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

func TestBP76RefreshWaiterCanCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"data":{"accessToken":"new-a","refreshToken":"new-r","expiresIn":3600}}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	a := accountWithRefreshToken()
	owner := make(chan error, 1)
	go func() { owner <- d.RefreshNow(context.Background(), a) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := d.RefreshNow(ctx, accountWithRefreshToken())
	close(release)
	if err != context.DeadlineExceeded {
		t.Fatalf("waiter error=%v", err)
	}
	if err := <-owner; err != nil {
		t.Fatalf("waiter canceled owner: %v", err)
	}
}

func TestBP76RefreshOwnerCancelDoesNotCancelWaiter(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
		}
		<-release
		fmt.Fprint(w, `{"data":{"accessToken":"new-a","refreshToken":"new-r","expiresIn":3600}}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	ctx, cancel := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() { owner <- d.RefreshNow(ctx, accountWithRefreshToken()) }()
	<-entered
	waiter := make(chan error, 1)
	go func() { waiter <- d.RefreshNow(context.Background(), accountWithRefreshToken()) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)
	close(release)
	<-owner
	if err := <-waiter; err != nil {
		t.Fatalf("one caller canceled shared refresh: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("cancel caused token replay: %d", hits.Load())
	}
}

func TestBP76RefreshFailureIsNotCached(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"data":{"accessToken":"new-a","refreshToken":"new-r","expiresIn":3600}}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	a := accountWithRefreshToken()
	if err := d.RefreshNow(context.Background(), a); err == nil {
		t.Fatal("expected first failure")
	}
	if err := d.RefreshNow(context.Background(), a); err != nil {
		t.Fatalf("failure cached: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatal(hits.Load())
	}
}

func TestBP76UnrotatedRefreshTokenStillAllowsForcedRefresh(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"data":{"accessToken":"new-a","expiresIn":3600}}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	a := accountWithRefreshToken()
	for i := 0; i < 2; i++ {
		if err := d.RefreshNow(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 2 {
		t.Fatal("forced refresh incorrectly reused unchanged refresh token result")
	}
}
