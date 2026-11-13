package lobster

// Text-to-image / image editing on the LobsterAI media API (BP-306).
// Endpoint: POST {server}/api/media/images/generate — private media service,
// verified live 2026-11-10 (qwen-image-3.0 t2i → succeeded, 18 credits).
// Parameter facts: /api/media/images/models parameterSpec (vendor machine
// readable): n 1-6, size enum (star-separated WxH), reference images 0-3
// ≤10MB, watermark default true.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

type mediaImageRequest struct {
	Model  string         `json:"model"`
	Type   string         `json:"type"` // t2i | i2i
	Prompt string         `json:"prompt"`
	Params mediaImageVars `json:"params"`
}

type mediaImageVars struct {
	N           int      `json:"n,omitempty"`
	Size        string   `json:"size,omitempty"`
	AspectRatio string   `json:"aspectRatio,omitempty"`
	Images      []string `json:"images,omitempty"`
	Watermark   *bool    `json:"watermark,omitempty"`
}

type mediaTaskResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
	Data struct {
		TaskID           any      `json:"taskId"`
		Status           string   `json:"status"`
		ResultURLs       []string `json:"resultUrls"`
		ErrorCode        any      `json:"errorCode"`
		ErrorMessage     string   `json:"errorMessage"`
		EstimatedCredits float64  `json:"estimatedCredits"`
	} `json:"data"`
}

// GenerateImage implements driver.ImageGenerator (single-image call; the
// gateway fans n>1 out as repeated calls, mirroring the antigravity path).
func (d *Driver) GenerateImage(ctx context.Context, acc *model.Account, modelName, prompt string) (*driver.ImageResult, error) {
	return d.GenerateImageFull(ctx, acc, modelName, prompt, 1, "", "", nil, nil)
}

// GenerateImageFull is the gateway-facing entry with clamped parameters.
// size/aspectRatio arrive already validated against the model's enum; refs
// non-empty flips the task type to i2i (image editing / multi-reference).
// watermark is nil (vendor default true) unless the caller sends it.
func (d *Driver) GenerateImageFull(ctx context.Context, acc *model.Account, modelName, prompt string, n int, size, aspectRatio string, watermark *bool, refs []string) (*driver.ImageResult, error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "lobster media: %v", err)
	}
	kind := "t2i"
	if len(refs) > 0 {
		kind = "i2i"
	}
	body := mediaImageRequest{
		Model: modelName, Type: kind, Prompt: prompt,
		Params: mediaImageVars{N: n, Size: size, AspectRatio: aspectRatio, Images: refs, Watermark: watermark},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "lobster media: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL("/media/images/generate"), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster image: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, payload)
	}
	var out mediaTaskResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, uperr.New(uperr.ClassInternal, 500, "lobster image: bad body: %v", err)
	}
	if out.Code != 0 {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "lobster image: code=%d %s", out.Code, out.Msg)
	}
	if t := strings.TrimSpace(out.Data.ErrorMessage); t != "" || out.Data.ErrorCode != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "lobster image: %s", t)
	}
	switch out.Data.Status {
	case "succeeded":
	case "failed", "cancelled", "timeout":
		return nil, uperr.New(uperr.ClassInternal, 500, "lobster image: task %s: %s", out.Data.Status, out.Data.ErrorMessage)
	default:
		// queued/processing would need the polling lane; the models verified so
		// far answer synchronously. Surface honestly instead of blocking.
		return nil, uperr.New(uperr.ClassInternal, 500,
			"lobster image: async status %q (task %v) — sync lane expected", out.Data.Status, out.Data.TaskID)
	}
	res := &driver.ImageResult{}
	for _, u := range out.Data.ResultURLs {
		if u != "" {
			res.Images = append(res.Images, driver.ImagePart{MIME: "image/png", URL: u})
		}
	}
	if len(res.Images) == 0 {
		return nil, uperr.New(uperr.ClassInternal, 500, "lobster image: no result URLs")
	}
	return res, nil
}
