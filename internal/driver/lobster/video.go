package lobster

// Text-to-video / image-to-video on the LobsterAI media API (BP-306).
// Submit: POST /api/media/videos/generate → task id (async).
// Poll:   GET  /api/media/videos/tasks/{id}   (the only query path that
//         answered 200 live; /api/media/v2/tasks/{id} reports "media task
//         does not exist" — verified 2026-11-10).
// Params come from /api/media/videos/models parameterSpec: resolution enum,
// duration 4-15, aspectRatio enum, firstFrame/lastFrame, referenceImages ≤9.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

type mediaVideoRequest struct {
	Model  string         `json:"model"`
	Type   string         `json:"type"` // t2v | i2v
	Prompt string         `json:"prompt"`
	Params mediaVideoVars `json:"params"`
}

type mediaVideoVars struct {
	Duration    int     `json:"durationSeconds,omitempty"`
	Resolution  string  `json:"resolution,omitempty"`
	AspectRatio string  `json:"aspectRatio,omitempty"`
	// Seedance's parameterSpec names the ratio field `ratio` (MiniMax-H3 uses
	// `aspectRatio`); both carry the same value so either validator is satisfied.
	Ratio      string  `json:"ratio,omitempty"`
	FirstFrame string  `json:"firstFrame,omitempty"`
	LastFrame  string  `json:"lastFrame,omitempty"`
	Watermark  *bool   `json:"watermark,omitempty"`
}

type mediaVideoStatus struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
	Data struct {
		TaskID           any      `json:"taskId"`
		Status           string   `json:"status"`
		ResultURLs       []string `json:"resultUrls"`
		Progress         int      `json:"progress"`
		EstimatedCredits float64  `json:"estimatedCredits"`
		ErrorMessage     string   `json:"errorMessage"`
	} `json:"data"`
}

// GenerateVideo submits one video task and polls it to terminal state. The
// context bounds the whole wait; callers pass a generous deadline (MiniMax-H3
// rendered a 4s/768P clip in ~70s live; seedance-2-5 allows 30s clips).
func (d *Driver) GenerateVideo(ctx context.Context, acc *model.Account, modelName, prompt string, resolution string, duration int, aspectRatio, firstFrame, lastFrame string) (*driver.VideoResult, error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "lobster media: %v", err)
	}
	kind := "t2v"
	if firstFrame != "" {
		kind = "i2v"
	}
	vars := mediaVideoVars{Duration: duration, Resolution: resolution, AspectRatio: aspectRatio}
	if strings.HasPrefix(modelName, "doubao-seedance") {
		vars.Ratio = aspectRatio // this family's parameterSpec names the field `ratio`
	}
	vars.FirstFrame = firstFrame
	vars.LastFrame = lastFrame
	raw, err := json.Marshal(mediaVideoRequest{Model: modelName, Type: kind, Prompt: prompt, Params: vars})
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "lobster media: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL("/media/videos/generate"), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster video: %v", err)
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, payload)
	}
	var sub mediaVideoStatus
	if err := json.Unmarshal(payload, &sub); err != nil {
		return nil, uperr.New(uperr.ClassInternal, 500, "lobster video: bad body: %v", err)
	}
	if sub.Code != 0 {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "lobster video: code=%d %s", sub.Code, sub.Msg)
	}
	taskID := fmt.Sprint(sub.Data.TaskID)
	if taskID == "" || taskID == "<nil>" {
		return nil, uperr.New(uperr.ClassInternal, 500, "lobster video: no task id: %s", sub.Data.ErrorMessage)
	}
	// Poll — the task is bound to THIS account's token (vendor requirement).
	poll := d.apiURL("/media/videos/tasks/" + taskID)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, uperr.New(uperr.ClassCanceled, 0, "lobster video: canceled while waiting (task %s)", taskID)
		case <-tick.C:
		}
		preq, err := http.NewRequestWithContext(ctx, http.MethodGet, poll, nil)
		if err != nil {
			return nil, err
		}
		preq.Header.Set("Accept", "application/json")
		preq.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		presp, err := d.rpc.Do(preq)
		if err != nil {
			continue // transient poll errors: keep waiting within ctx budget
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 1<<20))
		presp.Body.Close()
		var st mediaVideoStatus
		if json.Unmarshal(pbody, &st) != nil || st.Code != 0 {
			continue
		}
		switch st.Data.Status {
		case "succeeded":
			out := &driver.VideoResult{TaskID: taskID}
			for _, u := range st.Data.ResultURLs {
				if u != "" {
					out.URLs = append(out.URLs, u)
				}
			}
			if len(out.URLs) == 0 {
				return nil, uperr.New(uperr.ClassInternal, 500, "lobster video: succeeded with no URLs")
			}
			return out, nil
		case "failed", "cancelled", "timeout":
			return nil, uperr.New(uperr.ClassInternal, 500,
				"lobster video: task %s: %s", st.Data.Status, st.Data.ErrorMessage)
		}
	}
}
