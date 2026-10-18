package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBP76ProbeRejectsChatStreamError(t *testing.T) {
	raw := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial answer\"}}]}\n\ndata: {\"error\":{\"type\":\"upstream_error\",\"message\":\"upstream disconnected\"}}\n\ndata: [DONE]\n\n")
	out, err := parseChatStream(raw)
	if err == nil {
		t.Fatalf("conformance probe accepted a failed stream: text=%q; want non-nil error", out.text)
	}
}

func TestBP76ProbeRejectsAnthropicStreamError(t *testing.T) {
	raw := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream disconnected\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	out, err := parseAnthropicStream(raw)
	if err == nil {
		t.Fatalf("conformance probe accepted a failed stream: text=%q; want non-nil error", out.text)
	}
}

func TestBP76StreamScenarioRejectsPartialFailedReply(t *testing.T) {
	cases := []struct {
		proto proto
		body  string
	}{
		{protoChat, "data: {\"choices\":[{\"delta\":{\"content\":\"partial answer\"}}]}\n\ndata: {\"error\":{\"type\":\"upstream_error\",\"message\":\"disconnected\"}}\n\ndata: [DONE]\n\n"},
		{protoAnthropic, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"disconnected\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	}
	for _, tc := range cases {
		t.Run(string(tc.proto), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			if err := runScenario(srv.URL, "audit-model", tc.proto, "stream", time.Second); err == nil {
				t.Fatal("matrix scenario returned PASS for HTTP-200 stream containing an explicit error after partial output; want FAIL")
			}
		})
	}
}
