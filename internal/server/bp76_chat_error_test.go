package server_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBP76ChatStreamExposesUpstreamErrorAsError(t *testing.T) {
	up := newFakeUpstream(t)
	up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial answer\"}]}}]}}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: {\"error\":{\"code\":503,\"message\":\"audit upstream unavailable\",\"status\":\"UNAVAILABLE\"}}\n\n")
	})
	srv, _ := buildTestServer(t, up, 1)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gemini-3-pro","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("upstream error is not emitted as an error object; it is indistinguishable from model-generated content: %s", raw)
	}
	if strings.Contains(string(raw), `"content":"\n[stream error:`) {
		t.Fatalf("transport error was inserted into assistant message content: %s", raw)
	}
}
