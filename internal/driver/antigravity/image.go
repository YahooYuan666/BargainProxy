package antigravity

// Text-to-image on the antigravity channel. The upstream serves image models
// through the SAME v1internal stream endpoint as chat — the difference is all
// in the payload (requestType "image_gen", forced 4K image size, no thinking,
// no session, no tools) and in what comes back: candidates carry inlineData
// parts instead of text. Behaviour spec (clean-room, GPLv3 reference — shapes
// and flow only): research/2026-10-29-antigravity生图通道迁入.md.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// imageSSELineCap: a 4K JPEG arrives as ONE SSE line carrying ~13.3 MB of
// base64. The chat parser's 8 MB cap would cut exactly the frame we came for,
// so the image parser raises its own ceiling.
const imageSSELineCap = 64 << 20

// isImageModel mirrors the upstream routing rule: any model whose name
// contains "image" is served as image_gen.
func isImageModel(name string) bool {
	return strings.Contains(strings.ToLower(name), "image")
}

// buildImageRequest assembles the image_gen payload: a single user text turn
// plus the generation config the image path expects. No sessionId (image
// requests are stateless), no tools, no maxOutputTokens.
func buildImageRequest(project, upstreamModel, prompt string) ([]byte, error) {
	inner := map[string]any{
		"contents": []geminiContent{{
			Role:  "user",
			Parts: []geminiPart{{Text: prompt}},
		}},
		"generationConfig": map[string]any{
			"imageConfig":    map[string]any{"imageSize": "4K"},
			"thinkingConfig": map[string]any{"includeThoughts": false},
		},
	}
	innerRaw, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	outer := v1InternalRequest{
		Project:     project,
		Model:       upstreamModel,
		RequestType: "image_gen",
		UserAgent:   "antigravity",
		RequestID:   imageGenRequestID(),
		Request:     innerRaw,
	}
	return json.Marshal(outer)
}

// imageGenRequestID follows the reference shape image_gen/<epoch_ms>/<uuid v4>/12.
func imageGenRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	u := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), u)
}

// GenerateImage produces images for one prompt on one account (the upstream
// call is one image per request; multi-image requests call this repeatedly).
func (d *Driver) GenerateImage(ctx context.Context, acc *model.Account, modelName, prompt string) (*driver.ImageResult, error) {
	creds, project, err := d.resolveCreds(ctx, acc)
	if err != nil {
		return nil, err
	}
	body, err := buildImageRequest(project, modelName, prompt)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "antigravity: build image payload: %v", err)
	}
	resp, err := d.doStream(ctx, creds.AccessToken, modelName, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("antigravity: image upstream error status=%d model=%s account=%s body=%s",
			resp.StatusCode, modelName, acc.ID, truncate(raw, 500))
		d.dumpFailure(acc.ID, modelName, resp.StatusCode, body, raw)
		return nil, classifyHTTP(resp.StatusCode, raw, modelName)
	}
	watched := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer watched.Close()
	return parseImageSSE(ctx, watched, modelName)
}

// parseImageSSE consumes the image_gen stream: inlineData parts become images,
// plain text becomes the refusal/explanation, promptFeedback is the safety
// verdict, and frame-level errors classify like chat's (same frame family).
func parseImageSSE(ctx context.Context, r io.Reader, upstreamModel string) (*driver.ImageResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), imageSSELineCap)

	var (
		images   []driver.ImagePart
		text     strings.Builder
		blocked  string
		blockMsg string
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "antigravity: image stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if line == "[DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		if fe := detectFrameError(payload, upstreamModel); fe != nil {
			return nil, fe
		}
		var frame struct {
			Response       geminiResponse    `json:"response"`
			Candidates     []geminiCandidate `json:"candidates"`
			PromptFeedback *promptFeedback   `json:"promptFeedback"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue // tolerate unknown frames
		}
		gr := frame.Response
		if len(gr.Candidates) == 0 && len(frame.Candidates) > 0 {
			gr.Candidates = frame.Candidates // unwrapped form
		}
		if pf := frame.PromptFeedback; pf != nil && pf.BlockReason != "" &&
			!strings.EqualFold(pf.BlockReason, "BLOCK_REASON_UNSPECIFIED") {
			blocked, blockMsg = pf.BlockReason, pf.BlockReasonMessage
		}
		for _, cand := range gr.Candidates {
			for _, part := range cand.Content.Parts {
				switch {
				case part.InlineData != nil && part.InlineData.Data != "":
					images = append(images, driver.ImagePart{
						MIME: part.InlineData.MIMEType,
						B64:  part.InlineData.Data,
					})
				case part.Text != "" && !part.Thought && text.Len() < 4096:
					text.WriteString(part.Text)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "antigravity: image stream read: %v", err)
	}
	if blocked != "" && len(images) == 0 {
		return nil, uperr.New(uperr.ClassContentBlocked, 400,
			"antigravity: image prompt blocked by safety filter (%s): %s", blocked, blockMsg)
	}
	if len(images) > 0 {
		return &driver.ImageResult{Images: images}, nil
	}
	if t := strings.TrimSpace(text.String()); t != "" {
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"antigravity: image generation rejected: %s", truncate([]byte(t), 500))
	}
	return nil, uperr.New(uperr.ClassInternal, 500, "antigravity: image generation produced no image")
}
