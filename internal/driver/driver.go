// Package driver defines the upstream channel contract. One Driver serves one
// channel; the gateway routes normalized IR requests through the driver and
// receives normalized IR events back. Drivers own their protocol details
// (auth refresh, payload shapes, SSE parsing) and classify failures into
// uperr classes.
package driver

import (
	"context"
	"time"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
)

// Emit receives normalized stream events. Returning an error aborts the
// stream (the error is surfaced to the caller).
type Emit func(ev ir.StreamEvent) error

// ChatCall carries everything a driver needs for one upstream request.
type ChatCall struct {
	Account       *model.Account
	UpstreamModel string // channel native model id
	Request       *ir.ChatRequest
}

// Driver is one upstream channel implementation.
type Driver interface {
	// Channel returns the channel identifier (model.Channel*).
	Channel() string

	// Stream performs the upstream call, emitting IR events. It returns the
	// fully aggregated response (drivers always stream upstream and aggregate
	// locally — several upstreams only support SSE). Errors are *uperr.Error.
	Stream(ctx context.Context, call ChatCall, emit Emit) (*ir.ChatResponse, error)

	// Models returns the channel's model list when discoverable, best-effort.
	Models(ctx context.Context) ([]string, error)
}

// ProbeResult is the outcome of a lightweight account probe (refresh tokens /
// fetch quota). Drivers that have no cheap probe return nil.
type ProbeResult struct {
	Windows map[string]model.Window
	Credits *model.Credits
	Note    string
}

// Prober is implemented by drivers that can refresh account state without a
// chat request (used by background tasks and the panel).
type Prober interface {
	Probe(ctx context.Context, acc *model.Account) (*ProbeResult, error)
}

// Package is one credit pack (balance batch) as reported by the upstream
// billing endpoint — feeds the panel's 国模积分情况 (expiry composition).
type Package struct {
	Name     string    `json:"name"`
	Remain   int64     `json:"remain"`
	Size     int64     `json:"size"`
	ExpireAt time.Time `json:"expire_at,omitempty"`
}

// PackagesProber lists an account's credit packs (optional driver capability).
type PackagesProber interface {
	CreditPackages(ctx context.Context, acc *model.Account) ([]Package, error)
}

// ImagePart is one generated picture: base64 (no data: prefix) plus its MIME.
type ImagePart struct {
	MIME string `json:"mime"`
	B64  string `json:"b64"`
}

// ImageResult is the outcome of one text-to-image upstream call. Images and
// Text are mutually exclusive in practice: an upstream that refuses the prompt
// explains itself in text instead of sending pixels.
type ImageResult struct {
	Images []ImagePart
	Text   string // refusal / explanation when no image came back
}

// ImageGenerator is implemented by drivers that can produce images from a
// text prompt (optional capability; served through /v1/images/generations).
type ImageGenerator interface {
	GenerateImage(ctx context.Context, acc *model.Account, modelName, prompt string) (*ImageResult, error)
}

// Refresher refreshes credentials (OAuth / device tokens).
type Refresher interface {
	Refresh(ctx context.Context, acc *model.Account) error
}

// LoginFlows describes interactive login support for the panel.
type LoginFlows struct {
	// SupportsDeviceFlow: panel can start a device/OAuth flow.
	SupportsDeviceFlow bool
	// StartLogin begins a flow; completion is reported via callback.
	StartLogin func(ctx context.Context, onDone func(credsJSON []byte, name string, err error)) (instructions string, err error)
}

// Interactive is implemented by drivers that support account onboarding from
// the panel.
type Interactive interface {
	LoginFlows() LoginFlows
}

// PoolSetter is implemented by drivers that need pool access (quota updates
// on workers). Optional.
type PoolSetter interface {
	SetPool(p *pool.Pool)
}
