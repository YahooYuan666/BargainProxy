package qoder

import (
	"net/http"
	"testing"
)

// BP-129: the desktop header family swaps clienttype/version/UA/scene/product
// in place; everything else (COSY signature, machine identity) must survive.
func TestApplyDesktopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("cosy-clienttype", "5")
	h.Set("cosy-version", "0.1.43")
	h.Set("user-agent", "Go-http-client/2.0")
	h.Set("cosy-machineid", "mid")
	applyDesktopHeaders(h, false)
	if h.Get("cosy-clienttype") != "5" {
		t.Fatal("CLI family must be untouched when desktop mode is off")
	}
	applyDesktopHeaders(h, true)
	for k, want := range map[string]string{
		"cosy-clienttype":       "10",
		"cosy-version":          "1.1.69",
		"user-agent":            "undici",
		"cosy-scene":            "app",
		"cosy-business-product": "app",
		"cosy-machineid":        "mid",
	} {
		if h.Get(k) != want {
			t.Fatalf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
}
