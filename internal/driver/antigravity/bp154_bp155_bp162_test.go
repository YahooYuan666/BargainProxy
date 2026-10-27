package antigravity

import (
	"context"
	"io"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// bp154Stream runs parseSSE over an SSE body and returns the outcome.
func bp154Stream(t *testing.T, body string) (*ir.ChatResponse, error) {
	t.Helper()
	d := &Driver{}
	call := driver.ChatCall{
		UpstreamModel: "gemini-3-pro",
		Request:       &ir.ChatRequest{Model: "gemini-3-pro"},
	}
	return d.parseSSE(context.Background(), strings.NewReader(body), call, func(ir.StreamEvent) error { return nil })
}

// BP-154: a MALFORMED_FUNCTION_CALL terminal with nothing emitted must come
// back as a retryable error, not an empty success. The glitch arrives with
// no usable parts (sub2api #8362: "最后的错误帧可能没有 parts").
func TestBP154MalformedFunctionCallIsAnError(t *testing.T) {
	body := "data: {\"response\":{\"candidates\":[{\"finishReason\":\"MALFORMED_FUNCTION_CALL\"}]}}\n\n"
	resp, err := bp154Stream(t, body)
	if err == nil {
		t.Fatalf("malformed call returned a success response: %+v", resp)
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("error is not an uperr: %v", err)
	}
	if !RotatableForTest(ue.Class) {
		t.Fatalf("class %s is not rotatable — the gateway could not retry another account", ue.Class)
	}
}

// Once text reached the client the glitch can only end cleanly.
func TestBP154MalformedAfterTextEndsCleanly(t *testing.T) {
	body := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"finishReason\":\"MALFORMED_FUNCTION_CALL\"}]}}\n\n"
	resp, err := bp154Stream(t, body)
	if err != nil {
		t.Fatalf("after emission the glitch must not error: %v", err)
	}
	if resp.Message.Content[0].Text != "partial" {
		t.Fatalf("text lost: %q", resp.Message.Content[0].Text)
	}
}

// BP-155: a safety block arrives with no candidates and no error object. It
// must be a content block (no account penalty, no rotation), not a transport
// failure.
func TestBP155PromptFeedbackBlockIsContentBlocked(t *testing.T) {
	body := "data: {\"response\":{\"promptFeedback\":{\"blockReason\":\"SAFETY\",\"blockReasonMessage\":\"blocked by safety\"}}}\n\n"
	_, err := bp154Stream(t, body)
	if err == nil {
		t.Fatal("safety block passed through as success")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("error is not an uperr: %v", err)
	}
	if ue.Class != uperr.ClassContentBlocked {
		t.Fatalf("class = %s, want content_blocked", ue.Class)
	}
	if !strings.Contains(ue.Message, "SAFETY") {
		t.Fatalf("message lost the reason: %q", ue.Message)
	}
}

// A clean promptFeedback (no block) must not be read as a block.
func TestBP155CleanPromptFeedbackIgnored(t *testing.T) {
	body := "data: {\"response\":{\"promptFeedback\":{\"blockReason\":\"BLOCK_REASON_UNSPECIFIED\"},\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}}\n\n"
	resp, err := bp154Stream(t, body)
	if err != nil {
		t.Fatalf("unspecified blockReason errored: %v", err)
	}
	if resp.Message.Content[0].Text != "hi" {
		t.Fatalf("text lost: %q", resp.Message.Content[0].Text)
	}
}

// BP-162: the Argon band maps to its -medium upstream id and asks for the
// CLI identity.
func TestBP162ArgonMappingAndUA(t *testing.T) {
	if got := upstreamModelName("gemini-4-argon"); got != "gemini-4-argon-medium" {
		t.Fatalf("argon mapped to %q", got)
	}
	if got := uaForModel("gemini-4-argon"); got != argonCLIUA {
		t.Fatalf("argon UA = %q", got)
	}
	if got := uaForModel("gemini-3-pro"); got != antigravityUA {
		t.Fatalf("regular model UA = %q, want the IDE identity", got)
	}
}

// RotatableForTest mirrors uperr.Rotatable without importing the pool.
func RotatableForTest(class string) bool {
	switch class {
	case uperr.ClassBadRequest, uperr.ClassContentBlocked, uperr.ClassSentinel, uperr.ClassUpstreamBusy,
		uperr.ClassUpstreamTimeout, uperr.ClassCanceled, uperr.ClassNotFound, uperr.ClassEdgeBlock,
		uperr.ClassTransportDead, uperr.ClassAccountRestricted:
		return false
	}
	return true
}

var _ io.Reader = (*strings.Reader)(nil)
