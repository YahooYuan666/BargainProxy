package gateway

// Video task orchestration (BP-306): resolve a canonical video model to a
// channel, run the generation on one account in the background, and let the
// caller poll. Tasks live in memory — video jobs are minutes-scale, and the
// poll URL must keep working across restarts only for as long as the vendor
// task does; persistence is a follow-up if field usage wants it.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/media"
	"bargainproxy/internal/uperr"
)

// VideoTask is one pollable video job.
type VideoTask struct {
	ID       string          `json:"id"`
	ModelID  string          `json:"model"`
	Channel  string          `json:"channel,omitempty"`
	Account  string          `json:"account,omitempty"`
	Status   string          `json:"status"` // queued | processing | completed | failed
	Error    string          `json:"error,omitempty"`
	URLs     []string        `json:"urls,omitempty"`
	MIME     string          `json:"mime,omitempty"`
	Attempts int             `json:"attempts,omitempty"`
	created  time.Time
}

// videoTasks is the process-wide registry. Keyed by task id; a poll for an
// unknown id answers 404 (no cross-account discovery, matching the vendors).
type videoTasks struct {
	mu    sync.Mutex
	seq   int
	tasks map[string]*VideoTask
}

var tasks = &videoTasks{tasks: map[string]*VideoTask{}}

func (t *videoTasks) create(modelID string) *VideoTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	task := &VideoTask{ID: fmt.Sprintf("vtask-%d-%d", time.Now().Unix(), t.seq), ModelID: modelID, Status: "queued", created: time.Now()}
	t.tasks[task.ID] = task
	return task
}

func (t *videoTasks) get(id string) (*VideoTask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task, ok := t.tasks[id]
	return task, ok
}

// MaxVideoWait bounds one video job: seedance-2-5 allows 30s clips with
// render queues of several minutes; MiniMax-H3 rendered 4s/768P in ~70s live.
const MaxVideoWait = 20 * time.Minute

// GenerateVideo resolves the model, kicks the work on a pool account in a
// background goroutine, and returns the pollable task immediately.
func (g *Gateway) GenerateVideo(ctx context.Context, modelID, preferred, prompt, resolution, aspectRatio, firstFrame, lastFrame string, duration int) (*VideoTask, error) {
	m, ok := media.ByID(modelID)
	if !ok || m.Kind != "video" {
		return nil, uperr.New(uperr.ClassNotFound, 404,
			"unknown video model %q — see GET /v1/media/models", modelID)
	}
	if m.Browser {
		return nil, uperr.New(uperr.ClassModelUnavailable, 501,
			"model %s needs the browser channel, which is not wired yet (available=false)", m.ID)
	}
	channel, upstream, ok := media.Route(m, preferred)
	if !ok {
		return nil, uperr.New(uperr.ClassModelUnavailable, 501,
			"model %s has no servable channel yet", m.ID)
	}
	vg, ok := g.drivers[channel].(driver.VideoGenerator)
	if !ok {
		return nil, uperr.New(uperr.ClassModelUnavailable, 501,
			"channel %s does not implement video generation", channel)
	}
	if !g.pool.HasAccounts(channel) {
		ue := uperr.New(uperr.ClassQuotaExhausted, 429,
			"no account on channel %s; import one first", channel)
		ue.Channel = channel
		return nil, ue
	}
	// Enum discipline: capability gaps answer with models_that_can BEFORE any
	// account is touched (BP-306 §4.1 — a retry on the same model cannot fix it).
	capabilityCheck := func(param, val string, values []string, supported bool) error {
		if val == "" || supported {
			return nil
		}
		return uperr.New(uperr.ClassBadRequest, 400, capabilityMsg(m.ID, param))
	}
	for _, c := range []struct {
		param, val string
	}{
		{"resolution", resolution},
		{"aspect_ratio", aspectRatio},
	} {
		var values []string
		supported := true
		for _, p := range m.Params {
			if p.Name == c.param {
				values, supported = p.Values, p.Supported
			}
		}
		if err := capabilityCheck(c.param, c.val, values, supported); err != nil {
			return nil, err
		}
	}
	// Vendor rule lastFrameRequiresFirstFrame: a tail frame alone is a request
	// bug, answer before any account is touched.
	if lastFrame != "" && firstFrame == "" {
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"model %s: last_frame requires first_frame (vendor rule lastFrameRequiresFirstFrame)", m.ID)
	}

	task := tasks.create(m.ID)
	go func() {
		wctx, cancel := context.WithTimeout(context.Background(), MaxVideoWait)
		defer cancel()
		task.Status = "processing"
		var lastErr error
		for attempt := 1; attempt <= maxAccountAttempts; attempt++ {
			task.Attempts = attempt
			lease, err := g.pool.Acquire(wctx, channel, upstream, "")
			if err != nil {
				lastErr = g.classifyPoolError(err, channel, upstream)
				break
			}
			task.Account = lease.Account.Nickname
			task.Channel = channel
			res, gerr := vg.GenerateVideo(wctx, lease.Account, upstream, prompt, resolution, duration, aspectRatio, firstFrame, lastFrame)
			ue, hasUE := uperr.As(gerr)
			if hasUE && ue.Model == "" {
				ue.Model = upstream
			}
			if hasUE {
				g.pool.ApplyResult(lease.Account, upstream, ue)
			}
			lease.Release()
			if gerr == nil {
				task.Status = "completed"
				task.URLs = res.URLs
				task.MIME = res.MIME
				return
			}
			lastErr = gerr
			if wctx.Err() != nil || !hasUE || !uperr.Rotatable(ue.Class) {
				break
			}
		}
		task.Status = "failed"
		if lastErr != nil {
			task.Error = lastErr.Error()
		}
	}()
	return task, nil
}

// capabilityMsg renders the switch-model message shared by the video path.
func capabilityMsg(modelID, param string) string {
	ids := media.CapabilityModels("video", param)
	if len(ids) == 0 {
		return modelID + " does not support " + param
	}
	out := modelID + " does not support " + param + "; models that can: "
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}

// VideoTask returns a pollable task by id (unknown ids read as 404 upstream).
func (g *Gateway) VideoTask(id string) (*VideoTask, bool) { return tasks.get(id) }
