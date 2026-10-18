package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBP76GeminiStreamingToolArgumentsSurvive(t *testing.T) {
	up := newFakeUpstream(t)
	up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Paris\"}}}]},\"finishReason\":\"STOP\"}]}}\n\n")
	})
	srv, _ := buildTestServer(t, up, 1)
	body := `{"contents":[{"role":"user","parts":[{"text":"Weather in Paris?"}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]}`
	resp, err := http.Post(srv.URL+"/v1beta/models/gemini-3-pro:streamGenerateContent?alt=sse", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"city":"Paris"`) {
		t.Fatalf("upstream tool args were lost in Gemini native output: %s", raw)
	}
}

func TestBP76GeminiReplayPreservesToolHistory(t *testing.T) {
	up := newFakeUpstream(t)
	received := make(chan string, 1)
	up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("upstream body: %v", err)
		}
		raw, _ := json.Marshal(body)
		received <- string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
	})
	srv, _ := buildTestServer(t, up, 1)
	body := `{"contents":[{"role":"user","parts":[{"text":"Weather in Paris?"}]},{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"temperature":18,"audit_marker":"tool-result-must-survive"}}}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]}`
	resp, err := http.Post(srv.URL+"/v1beta/models/gemini-3-pro:generateContent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	wire := <-received
	if !strings.Contains(wire, `"functionCall"`) || !strings.Contains(wire, `"functionResponse"`) || !strings.Contains(wire, "tool-result-must-survive") {
		t.Fatalf("valid native Gemini tool replay was removed before upstream request: %s", wire)
	}
}
