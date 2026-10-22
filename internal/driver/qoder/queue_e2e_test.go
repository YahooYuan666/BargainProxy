package qoder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// decodeGatewayBody reverses gatewayEncode, so a test can read what the driver
// actually put on the wire.
func decodeGatewayBody(t *testing.T, encoded string) map[string]any {
	t.Helper()
	std := make([]byte, len(encoded))
	for i, ch := range []byte(encoded) {
		if !c2sOK[ch] {
			t.Fatalf("char out of alphabet: %q", ch)
		}
		std[i] = c2s[ch]
	}
	n := len(std)
	a := n / 3
	orig := make([]byte, n)
	copy(orig[:a], std[n-a:])
	copy(orig[a:n-a], std[a:n-a])
	copy(orig[n-a:], std[:a])
	plain, err := base64.StdEncoding.DecodeString(string(orig))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(plain, &doc); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, plain)
	}
	return doc
}

// TestQueuePollResendsSameRequestSetID pins BP-92: after a 10605 the driver
// asks the queue-status endpoint, and once the queue says the request is no
// longer queued it resends with the SAME request_set_id. A fresh id would make
// Qoder treat the resend as a brand-new request at the back of the queue —
// which is exactly why the queued binding kept failing before.
func TestQueuePollResendsSameRequestSetID(t *testing.T) {
	var mu sync.Mutex
	var setIDs []string
	statusCalls := 0
	chatCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v3/user/jobToken":
			fmt.Fprint(w, jobTokenJSON)
		case "/algo/api/v2/model/list":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"fallback"}`)
		case queueStatusPath:
			mu.Lock()
			statusCalls++
			n := statusCalls
			mu.Unlock()
			// First ask: still queued. Second: released.
			if n == 1 {
				fmt.Fprint(w, `{"isQueued":true,"retryAfterSeconds":0.5}`)
				return
			}
			fmt.Fprint(w, `{"isQueued":false}`)
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			raw, _ := io.ReadAll(r.Body)
			doc := decodeGatewayBody(t, string(raw))
			mu.Lock()
			chatCalls++
			n := chatCalls
			if id, ok := doc["request_set_id"].(string); ok {
				setIDs = append(setIDs, id)
			}
			mu.Unlock()
			if n == 1 {
				// The queue's answer: a 403 carrying the queue signal and the
				// request_set_id it registered.
				w.WriteHeader(http.StatusForbidden)
				id := ""
				mu.Lock()
				if len(setIDs) > 0 {
					id = setIDs[0]
				}
				mu.Unlock()
				fmt.Fprintf(w, `{"code":"10605","isQueued":true,"retryAfterSeconds":1,"request_set_id":%q}`, id)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{Enabled: true, GatewayBase: srv.URL, OpenBase: srv.URL, MaxInFlight: 1}, nil)
	acc := &model.Account{ID: "qoder:u1", CredsJSON: []byte(`{"pat":"pt-test","machine_id":"m","machine_token":"mt","machine_type":"mtype"}`)}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "GLM-5.3",
		Request: &ir.ChatRequest{
			Model:    "GLM-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	var text strings.Builder
	if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (original + resend after the queue released)", chatCalls)
	}
	if statusCalls == 0 {
		t.Fatal("the queue status endpoint was never asked")
	}
	if len(setIDs) != 2 {
		t.Fatalf("captured %d request_set_ids, want 2", len(setIDs))
	}
	if setIDs[0] == "" || setIDs[0] != setIDs[1] {
		t.Fatalf("resend used a different request_set_id (%q -> %q); Qoder would treat it as a new request",
			setIDs[0], setIDs[1])
	}
}

// TestQueuePollFallsBackWhenStatusMissing pins the fail-open contract: with no
// status endpoint (404) the driver still resends after the plain sleep, so the
// queue protocol can never make a request fail that used to work.
func TestQueuePollFallsBackWhenStatusMissing(t *testing.T) {
	var mu sync.Mutex
	chatCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v3/user/jobToken":
			fmt.Fprint(w, jobTokenJSON)
		case "/algo/api/v2/model/list":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"fallback"}`)
		case queueStatusPath:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"msg":"no such route"}`)
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			mu.Lock()
			chatCalls++
			n := chatCalls
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				// A tiny retryAfterSeconds keeps the fallback sleep short.
				fmt.Fprint(w, `{"code":"10605","isQueued":true,"retryAfterSeconds":1}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(config.QoderSettings{Enabled: true, GatewayBase: srv.URL, OpenBase: srv.URL, MaxInFlight: 1}, nil)
	acc := &model.Account{ID: "qoder:u1", CredsJSON: []byte(`{"pat":"pt-test","machine_id":"m","machine_token":"mt","machine_type":"mtype"}`)}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "GLM-5.3",
		Request: &ir.ChatRequest{
			Model:    "GLM-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	var text strings.Builder
	if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("stream must still succeed via the sleep fallback: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
}
