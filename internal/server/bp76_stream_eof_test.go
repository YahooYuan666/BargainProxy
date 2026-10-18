package server_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBP76AllIngressesRejectUpstreamEOFWithoutTerminal(t *testing.T) {
	cases := []struct {
		name          string
		path          string
		body          string
		stream        bool
		successMarker string
	}{
		{"chat-buffered", "/v1/chat/completions", `{"model":"gemini-3-pro","messages":[{"role":"user","content":"ping"}]}`, false, ""},
		{"anthropic-buffered", "/v1/messages", `{"model":"gemini-3-pro","max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`, false, ""},
		{"responses-buffered", "/v1/responses", `{"model":"gemini-3-pro","input":"ping"}`, false, ""},
		{"gemini-buffered", "/v1beta/models/gemini-3-pro:generateContent", `{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`, false, ""},
		{"chat-stream", "/v1/chat/completions", `{"model":"gemini-3-pro","stream":true,"messages":[{"role":"user","content":"ping"}]}`, true, `"finish_reason":"stop"`},
		{"anthropic-stream", "/v1/messages", `{"model":"gemini-3-pro","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`, true, `"stop_reason":"end_turn"`},
		{"responses-stream", "/v1/responses", `{"model":"gemini-3-pro","stream":true,"input":"ping"}`, true, `"status":"completed"`},
		{"gemini-stream", "/v1beta/models/gemini-3-pro:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`, true, `"finishReason":"STOP"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t)
			up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial answer\"}]}}]}}\n\n")
			})
			srv, _ := buildTestServer(t, up, 1)
			resp, err := http.Post(srv.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.stream {
				if resp.StatusCode == http.StatusOK {
					t.Fatalf("unterminated upstream stream became HTTP 200 success: %s", raw)
				}
			} else if strings.Contains(string(raw), tc.successMarker) && !strings.Contains(string(raw), `"error"`) {
				t.Fatalf("unterminated upstream stream became successful downstream terminal: %s", raw)
			}
		})
	}
}
