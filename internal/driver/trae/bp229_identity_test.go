package trae

import (
	"net/http"
	"strings"
	"testing"
)

// BP-229 (misakano traework 40ebfbaf/eb98b1b1, 2026-10-30 real client
// captures of Trae CN 1.107.1): the chat chain wears the full Chromium UA,
// appVersion 3.4.11 with version codes 20260924, and NO X-Cloudide-Token /
// X-Uid; the UG chain rides the CN package family.
func TestBP229SOLOHeadersMatchTheRealClient(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://agent.trae.cn/solo", nil)
	c := &Creds{AccessToken: "jwt", UID: "u1", DeviceID: "1234567890123456", MachineID: "mid"}
	soloHeaders(req, c, true)
	if got := req.Header.Get("User-Agent"); got != soloUserAgent || !strings.Contains(got, "TraeCN/1.107.1 Chrome/142.0.7444.235 Electron/39.2.7") {
		t.Fatalf("UA = %q, want the full Chromium-family string", got)
	}
	if got := req.Header.Get("X-Cloudide-Token"); got != "" {
		t.Fatalf("X-Cloudide-Token = %q; the chat chain does not send it (BP-229)", got)
	}
	if got := req.Header.Get("X-Uid"); got != "" {
		t.Fatalf("X-Uid = %q; the chat chain does not send it (BP-229)", got)
	}
	if got := req.Header.Get("X-App-Version"); got != "3.4.11" {
		t.Fatalf("X-App-Version = %q, want the CN build version", got)
	}
	for k, want := range map[string]string{
		"X-Ide-Version":      "3.4.11",
		"X-Ide-Version-Code": "20260924",
		"X-App-Version-Code": "20260924",
		"Accept":             "*/*",
		"Authorization":      "Cloud-IDE-JWT jwt",
		"X-Ide-Token":        "jwt",
		"X-Ide-Version-Type": "stable",
		"X-Machine-Id":       "mid",
		"X-Device-Id":        "1234567890123456",
	} {
		if got := req.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	// stream=false must produce the same shape — the stream flag rides the
	// body, not the headers.
	req2, _ := http.NewRequest("POST", "https://agent.trae.cn/solo", nil)
	soloHeaders(req2, c, false)
	if req2.Header.Get("Accept") != "*/*" {
		t.Fatalf("non-stream Accept = %q, want the same */*", req2.Header.Get("Accept"))
	}
}

// The UG chain gains the CN package family; region/device stay as the 9074
// fix requires.
func TestBP229UGHeadersCarryCNFamily(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.trae.cn/ug", nil)
	ugHeaders(req, &Creds{AccessToken: "jwt", DeviceID: "1234567890123456"})
	for k, want := range map[string]string{
		"Package-Type":     "stable_cn",
		"X-Lscbd-Aid":      "787976",
		"X-Lscbd-Platform": "windows",
		"X-SS-DP":          "787976",
		"App-Version":      "3.4.11",
		"X-User-Region":    "CN",
		"X-Device-Id":      "1234567890123456",
	} {
		if got := req.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}
