package workbuddy

// Video generation on the WorkBuddy channel (BP-306).
// Submit: POST {chatBase}/v2/videos/generations — body shape per the official
// client's buildConfiguredRequest (cli/dist/codebuddy-lite-wb.mjs):
//   {prompt, model, watermark:true, seconds, image_url?, last_image_url?,
//    extra_parameters:{resolution, aspect_ratio, enable_audio?}}
// Poll:   POST {chatBase}/v2/videos/tasks {task_id} — same-account only.
// Validation lives in the gateway/media layer; this file only speaks wire.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

type videoRequest struct {
	Prompt       string            `json:"prompt"`
	Model        string            `json:"model"`
	Watermark    bool              `json:"watermark"`
	Seconds      int               `json:"seconds,omitempty"`
	ImageURL     string            `json:"image_url,omitempty"`
	LastImageURL string            `json:"last_image_url,omitempty"`
	Extra        videoExtra        `json:"extra_parameters,omitempty"`
}

type videoExtra struct {
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	EnableAudio *bool  `json:"enable_audio,omitempty"`
}

type videoSubmitResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"data"`
}

type videoTaskResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Status string `json:"status"`
		Data   []struct {
			URL  string `json:"url"`
			MIME string `json:"mime_type,omitempty"`
		} `json:"data"`
	} `json:"data"`
}

// GenerateVideo implements driver.VideoGenerator: submit then poll to
// terminal state on the same account (cross-account queries are refused by
// the vendor).
func (d *Driver) GenerateVideo(ctx context.Context, acc *model.Account, modelName, prompt string, resolution string, duration int, aspectRatio, firstFrame, lastFrame string) (*driver.VideoResult, error) {
	creds, err := d.freshCreds(ctx, acc)
	if err != nil {
		return nil, err
	}
	if want, ok := videoRealm(modelName); ok && creds.RealmOrDefault() != want {
		return nil, uperr.New(uperr.ClassModelUnavailable, 0,
			"workbuddy: %s needs a %s account (have %s)", modelName, want, creds.RealmOrDefault())
	}
	body := videoRequest{
		Prompt: prompt, Model: modelName, Watermark: true, Seconds: duration,
		Extra: videoExtra{Resolution: resolution, AspectRatio: aspectRatio},
	}
	body.ImageURL = firstFrame
	body.LastImageURL = lastFrame // vendor: lastFrameRequiresFirstFrame — gateway rejects a lone tail first
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "workbuddy video: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.chatBase(creds)+"/v2/videos/generations", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	d.imageHeaderSet(req, creds)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy video: %v", err)
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, payload, modelName)
	}
	var sub videoSubmitResponse
	if err := json.Unmarshal(payload, &sub); err != nil {
		return nil, uperr.New(uperr.ClassInternal, 500, "workbuddy video: bad body: %v", err)
	}
	if sub.Code != 0 {
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"workbuddy video: code=%d %s", sub.Code, sub.Msg)
	}
	if sub.Data.ID == "" {
		return nil, uperr.New(uperr.ClassInternal, 500, "workbuddy video: no task id")
	}
	// Poll (same account token; 5s cadence, ctx bounds the wait).
	qb, _ := json.Marshal(map[string]string{"task_id": sub.Data.ID})
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, uperr.New(uperr.ClassCanceled, 0,
				"workbuddy video: canceled while waiting (task %s)", sub.Data.ID)
		case <-tick.C:
		}
		preq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.chatBase(creds)+"/v2/videos/tasks", bytes.NewReader(qb))
		if err != nil {
			return nil, err
		}
		preq.Header.Set("Content-Type", "application/json")
		preq.Header.Set("Accept", "application/json")
		d.imageHeaderSet(preq, creds)
		presp, err := d.rpc.Do(preq)
		if err != nil {
			continue
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 1<<20))
		presp.Body.Close()
		var st videoTaskResponse
		if json.Unmarshal(pbody, &st) != nil || st.Code != 0 {
			continue
		}
		switch st.Data.Status {
		case "completed":
			out := &driver.VideoResult{TaskID: sub.Data.ID, MIME: "video/mp4"}
			for _, v := range st.Data.Data {
				if v.URL != "" {
					out.URLs = append(out.URLs, v.URL)
				}
			}
			if len(out.URLs) == 0 {
				return nil, uperr.New(uperr.ClassInternal, 500, "workbuddy video: completed with no URLs")
			}
			return out, nil
		case "failed", "cancelled":
			return nil, uperr.New(uperr.ClassInternal, 500,
				"workbuddy video: task %s", st.Data.Status)
		}
	}
}

// videoRealm mirrors the two realms' model catalogs (Global /v3/config lists
// kling-video-v3 + hy-image-3.5; minimax-video-h3 is CN's default video model
// but present in both — realm gating applies to the image models only today).
func videoRealm(modelName string) (string, bool) {
	switch modelName {
	case "kling-video-v3":
		return "global", true
	}
	return "", false
}
