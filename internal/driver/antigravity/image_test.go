package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestBuildImageRequestShape pins the image_gen payload: the differences from
// a chat request are the whole feature (behaviour spec §1).
func TestBuildImageRequestShape(t *testing.T) {
	raw, err := buildImageRequest("proj-1", model.ImageGenModel, "一只戴帽子的猫")
	if err != nil {
		t.Fatal(err)
	}
	var outer struct {
		Project     string          `json:"project"`
		Model       string          `json:"model"`
		RequestType string          `json:"requestType"`
		RequestID   string          `json:"requestId"`
		Request     json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatal(err)
	}
	if outer.Project != "proj-1" || outer.Model != model.ImageGenModel {
		t.Fatalf("envelope project/model = %q/%q", outer.Project, outer.Model)
	}
	if outer.RequestType != "image_gen" {
		t.Fatalf("requestType = %q, want image_gen (chat uses agent)", outer.RequestType)
	}
	if !strings.HasPrefix(outer.RequestID, "image_gen/") || !strings.HasSuffix(outer.RequestID, "/12") {
		t.Fatalf("requestId = %q, want image_gen/<epoch>/<uuid>/12", outer.RequestID)
	}
	var inner struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
			SessionID string `json:"sessionId,omitempty"`
		} `json:"contents"`
		GenerationConfig struct {
			ImageConfig struct {
				ImageSize string `json:"imageSize"`
			} `json:"imageConfig"`
			ThinkingConfig struct {
				IncludeThoughts *bool `json:"includeThoughts"`
			} `json:"thinkingConfig"`
			MaxOutputTokens any `json:"maxOutputTokens"`
		} `json:"generationConfig"`
		Tools any `json:"tools"`
	}
	if err := json.Unmarshal(outer.Request, &inner); err != nil {
		t.Fatal(err)
	}
	if len(inner.Contents) != 1 || inner.Contents[0].Role != "user" ||
		len(inner.Contents[0].Parts) != 1 || inner.Contents[0].Parts[0].Text != "一只戴帽子的猫" {
		t.Fatalf("contents = %+v, want one user text turn with the prompt", inner.Contents)
	}
	if inner.Contents[0].SessionID != "" {
		t.Fatalf("sessionId = %q, image requests are stateless", inner.Contents[0].SessionID)
	}
	// The forced 4K is the one size parameter the upstream accepts; dropping
	// it changes what the upstream renders (mutation check).
	if inner.GenerationConfig.ImageConfig.ImageSize != "4K" {
		t.Fatalf("imageSize = %q, want forced 4K", inner.GenerationConfig.ImageConfig.ImageSize)
	}
	if inner.GenerationConfig.ThinkingConfig.IncludeThoughts == nil ||
		*inner.GenerationConfig.ThinkingConfig.IncludeThoughts {
		t.Fatal("includeThoughts must be explicitly false for image requests")
	}
	if inner.GenerationConfig.MaxOutputTokens != nil || inner.Tools != nil {
		t.Fatal("image requests carry no maxOutputTokens and no tools")
	}
}

// TestGenerateImageCollectsInlineData: the image rides candidates parts as
// inlineData; a 4K JPEG is one ~13MB SSE line, far past the chat parser's
// 8MB cap — the image parser must swallow it whole.
func TestGenerateImageCollectsInlineData(t *testing.T) {
	bigB64 := strings.Repeat("A", 11<<20) // ~11.5MB in ONE line
	var gotType, gotSize string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllBody(r)
		var env struct {
			RequestType string `json:"requestType"`
			Request     struct {
				GenerationConfig struct {
					ImageConfig struct {
						ImageSize string `json:"imageSize"`
					} `json:"imageConfig"`
				} `json:"generationConfig"`
			} `json:"request"`
		}
		_ = json.Unmarshal(body, &env)
		gotType, gotSize = env.RequestType, env.Request.GenerationConfig.ImageConfig.ImageSize
		w.Header().Set("Content-Type", "text/event-stream")
		// Wrapped form (response nested) with the big image…
		fmt.Fprintf(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"inlineData\":{\"mimeType\":\"image/jpeg\",\"data\":%q}}]}}]}}\n\n", bigB64)
		// …then the unwrapped form carrying a second part (multi-image).
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"c21hbGw="}}]}}]}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	res, err := d.GenerateImage(context.Background(), testAccount(t), model.ImageGenModel, "a cat")
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if gotType != "image_gen" || gotSize != "4K" {
		t.Fatalf("upstream saw requestType=%q imageSize=%q", gotType, gotSize)
	}
	if len(res.Images) != 2 {
		t.Fatalf("images = %d, want 2 (wrapped + unwrapped frames)", len(res.Images))
	}
	if res.Images[0].MIME != "image/jpeg" || len(res.Images[0].B64) != len(bigB64) {
		t.Fatalf("first image MIME/len = %s/%d (big frame truncated?)", res.Images[0].MIME, len(res.Images[0].B64))
	}
	if res.Images[1].MIME != "image/png" || res.Images[1].B64 != "c21hbGw=" {
		t.Fatalf("second image = %+v", res.Images[1])
	}
}

// TestGenerateImageRefusalBecomesBadRequest: a model that answers with words
// instead of pixels is a refusal — surfaced as 400 with the text (A2 parity).
func TestGenerateImageRefusalBecomesBadRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"I cannot draw that."}]},"finishReason":"STOP"}]}}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	_, err := d.GenerateImage(context.Background(), testAccount(t), model.ImageGenModel, "bad prompt")
	if err == nil {
		t.Fatal("a text-only answer must fail, not return zero images")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %v, want bad_request", err)
	}
	if !strings.Contains(ue.Message, "I cannot draw that.") {
		t.Fatalf("refusal text missing from %q", ue.Message)
	}
}

// TestGenerateImageSafetyBlock: promptFeedback.blockReason is the safety
// verdict (BP-155 shape) — content_blocked, not bad_request.
func TestGenerateImageSafetyBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked"}}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	_, err := d.GenerateImage(context.Background(), testAccount(t), model.ImageGenModel, "x")
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassContentBlocked {
		t.Fatalf("class = %v, want content_blocked", err)
	}
}

// TestGenerateImageFrameError: a mid-stream error frame on a 200 classifies
// like chat's (same frame family, ticket 36).
func TestGenerateImageFrameError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	_, err := d.GenerateImage(context.Background(), testAccount(t), model.ImageGenModel, "x")
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want classified upstream error, got %v", err)
	}
	if ue.Status != 429 {
		t.Fatalf("status = %d, want 429", ue.Status)
	}
}

// TestGenerateImageNoImageNoText: a silent clean stream is an upstream fault,
// not a client error.
func TestGenerateImageNoImageNoText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	_, err := d.GenerateImage(context.Background(), testAccount(t), model.ImageGenModel, "x")
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassInternal {
		t.Fatalf("class = %v, want internal (no image in response)", err)
	}
}

// TestGenerateImageUpstreamErrorStatus: non-200 goes through the same HTTP
// classifier as chat and keeps the upstream body in the server log.
func TestGenerateImageUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":400,"message":"invalid","status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	_, err := d.GenerateImage(context.Background(), testAccount(t), model.ImageGenModel, "x")
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want classified error, got %v", err)
	}
	if ue.Status != 400 {
		t.Fatalf("status = %d, want 400", ue.Status)
	}
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}
