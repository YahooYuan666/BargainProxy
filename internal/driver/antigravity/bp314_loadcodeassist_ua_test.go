package antigravity

// BP-314: loadCodeAssist must send the CLI User-Agent, or the upstream answers
// 200 with an EMPTY project id — silently, with no error and no message.
//
// Measured 2026-11-12 20:5x, one variable (same token, same host, all three
// accounts):
//
//	no UA  → cloudaicompanionProject: ""                    (both hosts)
//	with UA → cloudaicompanionProject: "aicode-consumers"   (both hosts)
//
// Consequence: discovery "succeeded" with "", the top-level project field was
// dropped by omitempty on every chat request, and the DB project_id column was
// empty for all three accounts. AIClient2API never hit this because its callApi
// attaches the UA to EVERY request uniformly; we had it on chat and on the
// quota probe but not here.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/config"
)

func TestLoadCodeAssistSendsCLIUserAgent(t *testing.T) {
	var gotUA, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		// Only the CLI identity gets the project; anything else is the silent
		// empty answer the upstream really gives.
		if gotUA == "" {
			fmt.Fprint(w, `{"cloudaicompanionProject":"","gcpManaged":false}`)
			return
		}
		fmt.Fprint(w, `{"cloudaicompanionProject":"aicode-consumers","gcpManaged":false}`)
	}))
	defer srv.Close()

	d := New(config.AntigravitySettings{Enabled: true, BaseURL: srv.URL}, &http.Client{})
	pid, err := d.loadCodeAssist(context.Background(), "ag-1", "token")
	if err != nil {
		t.Fatalf("loadCodeAssist: %v", err)
	}
	if gotUA == "" {
		t.Fatal("loadCodeAssist sent NO User-Agent; the upstream then answers an empty project and " +
			"discovery silently succeeds with nothing")
	}
	if gotUA != antigravityUA {
		t.Fatalf("User-Agent = %q, want the CLI identity %q", gotUA, antigravityUA)
	}
	if pid != "aicode-consumers" {
		t.Fatalf("project = %q, want aicode-consumers", pid)
	}
	// AIClient2API sends the IDE identity here, not the Code Assist CLI one.
	if gotBody != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Fatalf("body = %s, want the reference's ideType metadata", gotBody)
	}
}

// The quota probe already carried the UA; keep it that way (it is the same
// class of silent-empty failure if it ever regresses).
func TestQuotaProbeSendsCLIUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"groups":[{"buckets":[
			{"bucketId":"gemini-5h","remainingFraction":1,"resetTime":"2026-11-12T13:26:01Z"},
			{"bucketId":"gemini-weekly","remainingFraction":0.95,"resetTime":"2026-11-14T13:11:27Z"}]}]}`)
	}))
	defer srv.Close()

	d := New(config.AntigravitySettings{Enabled: true, BaseURL: srv.URL}, &http.Client{})
	if _, err := d.fetchQuotaHost(context.Background(), srv.URL, "token"); err != nil {
		t.Fatalf("fetchQuotaHost: %v", err)
	}
	if gotUA != antigravityUA {
		t.Fatalf("quota probe User-Agent = %q, want %q", gotUA, antigravityUA)
	}
}
