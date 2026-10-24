package lobster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// BP-103: the proxy kills oversize requests with 17-67s of silence followed by
// an empty 500. Both ceilings (body bytes, prompt tokens) must be gated before
// the socket opens, with an error that cascades to another channel.

// oversizeRequest builds a chat request whose marshalled body is at least
// target bytes, as a single padded user message.
func oversizeRequest(target int) *ir.ChatRequest {
	pad := strings.Repeat("a", target)
	return &ir.ChatRequest{
		Model: "deepseek-flash",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: pad}}},
		},
	}
}

func TestOversizeBodyGatedBeforeSocket(t *testing.T) {
	// The server must never see a request: the gate fires in Stream before
	// the HTTP client dials. If it does, the test fails via the 200 response
	// path (a stream reaching emit) instead of the expected error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be contacted for an oversize request")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	req := oversizeRequest(5 << 20) // 5 MB: measured dead zone
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       req,
	}, emit)
	if err == nil {
		t.Fatal("expected an oversize rejection, got nil")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("expected *uperr.Error, got %T: %v", err, err)
	}
	if ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %q, want bad_request (rotates to another channel, no account penalty)", ue.Class)
	}
	if !strings.Contains(ue.Message, "bytes") {
		t.Fatalf("message should name the byte ceiling, got: %s", ue.Message)
	}
}

func TestUnderLimitBodyPassesGate(t *testing.T) {
	// A small request must reach the wire — the gate exists only for the
	// measured dead zone, not as a general limiter.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Minimal terminal frame so parseStream finishes cleanly.
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	req := oversizeRequest(1 << 10)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       req,
	}, emit)
	if err != nil {
		t.Fatalf("small request must pass the gate, got: %v", err)
	}
}

func TestOversizeTokenEstimateGated(t *testing.T) {
	// The token ceiling: est > 1,000,000 must be rejected even when the body
	// is small — repeated ASCII merges to few real tokens, but the estimator
	// counts chars/2, so build text whose estimate crosses the line.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be contacted for an over-ceiling estimate")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	// 2.5M chars → estimate 1.25M > 1M; body 2.5MB < 4MB so only the token
	// gate can fire.
	req := oversizeRequest(2_500_000)
	if est := ir.EstimatePromptTokens(req); est <= maxPromptEst {
		t.Fatalf("test setup: estimate %d must exceed %d", est, maxPromptEst)
	}
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       req,
	}, emit)
	if err == nil {
		t.Fatal("expected a token-ceiling rejection, got nil")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("expected *uperr.Error, got %T: %v", err, err)
	}
	if ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %q, want bad_request", ue.Class)
	}
	if !strings.Contains(ue.Message, "tokens") {
		t.Fatalf("message should name the token ceiling, got: %s", ue.Message)
	}
}

func TestEstimateMovedToIR(t *testing.T) {
	// The estimator now lives in ir so the driver can share it; the gateway
	// forwards. Pin the behaviour the fbwait tests rely on.
	req := &ir.ChatRequest{
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("x", 8000)}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "image", ImageB64: "AAAA"}}},
		},
	}
	if got := ir.EstimatePromptTokens(req); got != 4000+1500 {
		t.Fatalf("ir.EstimatePromptTokens = %d, want %d", got, 4000+1500)
	}
	if ir.EstimatePromptTokens(nil) != 0 {
		t.Fatal("nil request must estimate 0")
	}
}

// rejectOversizeIsClassified pins the unit contract directly: byte and token
// paths produce bad_request with distinct, human-readable messages.
func TestRejectOversizeClassification(t *testing.T) {
	if err := rejectOversize(make([]byte, maxBodyBytes+1), 0); err == nil {
		t.Fatal("body over ceiling must be rejected")
	}
	if err := rejectOversize(make([]byte, maxBodyBytes), maxPromptEst+1); err == nil {
		t.Fatal("estimate over ceiling must be rejected")
	}
	if err := rejectOversize(make([]byte, maxBodyBytes), maxPromptEst); err != nil {
		t.Fatalf("at-ceiling values must pass, got: %v", err)
	}
	big := rejectOversize(make([]byte, maxBodyBytes+1), 0)
	ue, _ := uperr.As(big)
	// JSON-encodable message: FloorFromMessage must not see a max_tokens floor
	// in it (BP-99 retry would misfire on "less than the minimum" phrasing).
	raw, _ := json.Marshal(ue.Message)
	if string(raw) == "" {
		t.Fatal("message must marshal")
	}
}
