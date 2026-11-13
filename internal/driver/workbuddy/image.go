package workbuddy

// Text-to-image and image editing on the WorkBuddy channel (BP-306).
// Endpoint: POST {chatBase}/v2/images/generations — OpenAI-shaped, verified
// live on both realms (CN hunyuan-image-alpha, Global hy-image-3.5).
// Parameter discipline: upstream performs NO validation (n=0/99, size=banana
// all answer 200 and bill) — the gateway clamps before calling, see
// internal/media catalog notes and research/2026-11-10-媒体参数规格-唯一事实源.md §1.5.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

type imageRequest struct {
	Model  string   `json:"model"`
	Prompt string   `json:"prompt"`
	N      int      `json:"n,omitempty"`
	Size   string   `json:"size,omitempty"`
	Image  []string `json:"image,omitempty"`
}

type imageResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Data []struct {
			URL  string `json:"url"`
			Size string `json:"size"`
		} `json:"data"`
		Usage struct {
			Credit float64 `json:"credit"`
		} `json:"usage"`
	} `json:"data"`
}

// imageHeaderSet carries the fingerprint headers verified live in the BP-306
// probes (chat UA family + X-Product SaaS + X-Domain). The image endpoint
// answered 200 to exactly this shape on both realms.
func (d *Driver) imageHeaderSet(req *http.Request, creds *Creds) {
	global := creds.RealmOrDefault() == "global"
	req.Header.Set("User-Agent", workBuddyUA(global))
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	if !creds.IsAPIKey() {
		origin := "https://www.workbuddy.cn"
		if global {
			origin = "https://www.workbuddy.ai"
		}
		req.Header.Set("X-Domain", strings.TrimPrefix(origin, "https://"))
	}
	if creds.IsAPIKey() {
		req.Header.Set("X-API-Key", creds.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	}
	if creds.DeviceToken != "" {
		req.Header.Set("X-Device-Token", creds.DeviceToken)
	}
}

func (d *Driver) freshCreds(ctx context.Context, acc *model.Account) (*Creds, error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "workbuddy media: %v", err)
	}
	if !creds.accessTokenUsable(time.Now()) && !creds.IsAPIKey() {
		if rerr := d.Refresh(ctx, acc); rerr != nil {
			return nil, rerr
		}
		if creds, err = ParseCreds(acc.CredsJSON); err != nil {
			return nil, err
		}
	}
	return creds, nil
}

// mediaImage posts one image request and parses the envelope.
func (d *Driver) mediaImage(ctx context.Context, acc *model.Account, body any) (*driver.ImageResult, error) {
	creds, err := d.freshCreds(ctx, acc)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "workbuddy media: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.chatBase(creds)+"/v2/images/generations", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	d.imageHeaderSet(req, creds)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy image: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, payload, "")
	}
	var out imageResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, uperr.New(uperr.ClassInternal, 500, "workbuddy image: bad body: %v", err)
	}
	if out.Code != 0 {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "workbuddy image: code=%d %s", out.Code, out.Msg)
	}
	res := &driver.ImageResult{}
	for _, im := range out.Data.Data {
		if im.URL != "" {
			res.Images = append(res.Images, driver.ImagePart{MIME: "image/png", URL: im.URL, Size: im.Size})
		}
	}
	if len(res.Images) == 0 {
		return nil, uperr.New(uperr.ClassInternal, 500, "workbuddy image: no image in response")
	}
	return res, nil
}

// GenerateImage implements driver.ImageGenerator.
func (d *Driver) GenerateImage(ctx context.Context, acc *model.Account, modelName, prompt string) (*driver.ImageResult, error) {
	return d.GenerateImageFull(ctx, acc, modelName, prompt, 1, "", "", nil, nil)
}

// GenerateImageFull is the gateway-facing entry with the clamped parameters.
func (d *Driver) GenerateImageFull(ctx context.Context, acc *model.Account, modelName, prompt string, n int, size, aspectRatio string, watermark *bool, refs []string) (*driver.ImageResult, error) {
	// aspectRatio/watermark unused: no workbuddy image model declares
	// aspect_ratio, and the vendor stamps its own watermark client-side.
	_, _, _ = aspectRatio, watermark, size
	creds, err := ParseCreds(acc.CredsJSON)
	if err == nil {
		if want, ok := mediaRealm(modelName); ok && creds.RealmOrDefault() != want {
			return nil, uperr.New(uperr.ClassModelUnavailable, 0,
				"workbuddy: %s needs a %s account (have %s)", modelName, want, creds.RealmOrDefault())
		}
	}
	body := imageRequest{Model: modelName, Prompt: prompt, N: n, Image: refs}
	if size != "" {
		body.Size = size
	}
	return d.mediaImage(ctx, acc, body)
}

// mediaRealm pins which realm serves a model (hy-image is Global-only,
// hunyuan-image-alpha is CN-only — /v3/config per-realm catalogs differ).
func mediaRealm(modelName string) (string, bool) {
	switch modelName {
	case "hy-image-3.5":
		return "global", true
	case "hunyuan-image-alpha", "hunyuan-image-alpha-edit":
		return "cn", true
	}
	return "", false
}
