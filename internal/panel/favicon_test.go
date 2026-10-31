package panel_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/panel"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

func TestFaviconIsCryingFace(t *testing.T) {
	cfg := config.Default()
	st := store.NewMemory()
	p := pool.New(pool.Params{}, nil, nil)
	gw := gateway.New(cfg, p, router.New(cfg), nil, st)
	pan := panel.New(cfg, gw, st, nil)

	mux := http.NewServeMux()
	pan.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. Check GET /favicon.ico returns SVG containing 😭
	for _, path := range []string{"/favicon.ico", "/panel/favicon.ico"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d, want 200", path, resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "image/svg+xml") {
			t.Fatalf("GET %s Content-Type = %q, want image/svg+xml", path, ct)
		}
		resp.Body.Close()
	}

	// 2. Check index.html has the SVG favicon link and title
	resp, err := http.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("GET /panel/ failed: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	html := string(buf[:n])
	if !strings.Contains(html, `rel="icon"`) || !strings.Contains(html, "😭") {
		t.Fatalf("/panel/ index.html missing crying favicon link; got head:\n%.500s", html)
	}
	if !strings.Contains(html, "<title>😭 穷鬼自建中转</title>") {
		t.Fatalf("/panel/ index.html missing title; got head:\n%.500s", html)
	}
}
