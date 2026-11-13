package gateway

// Text-to-image orchestration (BP-186). The images path deliberately does NOT
// ride the chat machinery: no route table, no bindings, no first-byte window,
// no sentinel guard — just pick an antigravity account, call the driver, cool
// the account on failure, and try the next one. One upstream call makes one
// image, so an n>1 request is n serial calls.

import (
	"context"
	"log"
	"strings"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/media"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// ImageOutcome is the result of a completed images request.
type ImageOutcome struct {
	Images   []driver.ImagePart
	Account  *model.Account // last account that produced an image
	Attempts int            // total account attempts paid (per request, all images)
}

// imageChannel is the only channel with a text-to-image driver today.
const imageChannel = model.ChannelAntigravity

// ImageGenAvailable reports whether image generation can be served: the
// channel has a driver implementing driver.ImageGenerator.
// ChannelAlive reports whether a channel currently has an enabled driver and
// at least one importable account — the availability signal behind
// GET /v1/media/models (BP-306 P1).
func (g *Gateway) ChannelAlive(channel string) bool {
	if _, ok := g.drivers[channel]; !ok {
		return false
	}
	return g.pool.HasAccounts(channel)
}

func (g *Gateway) ImageGenAvailable() bool {
	_, ok := g.drivers[imageChannel].(driver.ImageGenerator)
	return ok
}

// GenerateImages produces n images for the prompt, rotating accounts per call.
// Errors are *uperr.Error; pool-side errors are mapped through the same
// classifier the chat path uses so HTTP statuses stay consistent.
func (g *Gateway) GenerateImages(ctx context.Context, modelName, prompt string, n int) (*ImageOutcome, error) {
	d := g.drivers[imageChannel]
	ig, ok := d.(driver.ImageGenerator)
	if !ok {
		return nil, uperr.New(uperr.ClassModelUnavailable, 501,
			"image generation is not available on channel %s", imageChannel)
	}
	if !g.pool.HasAccounts(imageChannel) {
		ue := uperr.New(uperr.ClassQuotaExhausted, 429,
			"no account on channel %s; import a gemini-antigravity account first", imageChannel)
		ue.Channel = imageChannel
		return nil, ue
	}

	out := &ImageOutcome{}
	for i := 0; i < n; i++ {
		call, err := g.generateOneImage(ctx, ig, modelName, prompt)
		out.Attempts += call.Attempts
		if err != nil {
			return out, err
		}
		out.Images = append(out.Images, call.Images...)
		out.Account = call.Account
	}
	return out, nil
}

// imageCall is one image's outcome plus who served it.
type imageCall struct {
	Images   []driver.ImagePart
	Account  *model.Account
	Attempts int
}

// generateOneImage runs one upstream image call with account rotation. A
// non-rotatable failure (bad request, safety block, upstream busy, canceled)
// ends the request: the same prompt against another account gets the same
// verdict, and walking the pool would only spread cooldowns. On failure the
// returned call still carries the Attempts tally for the request log.
func (g *Gateway) generateOneImage(ctx context.Context, ig driver.ImageGenerator, modelName, prompt string) (*imageCall, error) {
	calls := &imageCall{}
	for attempt := 1; attempt <= maxAccountAttempts; attempt++ {
		calls.Attempts = attempt
		lease, err := g.pool.Acquire(ctx, imageChannel, modelName, "")
		if err != nil {
			return calls, g.classifyPoolError(err, imageChannel, modelName)
		}
		res, gerr := ig.GenerateImage(ctx, lease.Account, modelName, prompt)
		if gerr == nil {
			lease.Release()
			calls.Images = res.Images
			calls.Account = lease.Account
			return calls, nil
		}
		ue, ok := uperr.As(gerr)
		if ok {
			// The failure is inherently scoped to this image model — make the
			// cooldown land on the model, not the whole account (an image-429
			// says nothing about the account's chat models, BP-116 spirit).
			if ue.Model == "" {
				ue.Model = modelName
			}
			g.pool.ApplyResult(lease.Account, modelName, ue)
		}
		lease.Release()
		log.Printf("gateway: image attempt %d/%d failed on %s: %v",
			attempt, maxAccountAttempts, lease.Account.Nickname, gerr)
		if ctx.Err() != nil {
			return calls, gerr
		}
		if !ok || !uperr.Rotatable(ue.Class) {
			return calls, gerr
		}
	}
	return calls, uperr.New(uperr.ClassQuotaExhausted, 429,
		"every antigravity account failed to serve the image request (%d attempts)", maxAccountAttempts)
}

// imageRequested reports whether a chat request names the image-only model —
// used to point callers at the images endpoint instead of a route miss.
func imageRequested(modelName string) bool {
	return strings.EqualFold(strings.TrimSpace(modelName), model.ImageGenModel)
}

// GenerateImagesModel is the BP-306 multi-channel entry: resolve a canonical
// media model to a channel, then reuse the antigravity path's account
// rotation. size/refs arrive pre-validated against the catalog (the server
// layer owns enum checks; the gateway owns clamping).
func (g *Gateway) GenerateImagesModel(ctx context.Context, modelID, preferred, prompt string, n int, size, aspectRatio string, watermark *bool, refs []string) (*ImageOutcome, error) {
	m, ok := media.ByID(modelID)
	if !ok || m.Kind != "image" {
		return nil, uperr.New(uperr.ClassNotFound, 404,
			"unknown image model %q — see GET /v1/media/models", modelID)
	}
	channel, upstream, ok := media.Route(m, preferred)
	if !ok {
		return nil, uperr.New(uperr.ClassModelUnavailable, 501,
			"model %s has no servable channel yet", m.ID)
	}
	ig, ok := g.drivers[channel].(driver.ImageGenerator)
	if !ok {
		return nil, uperr.New(uperr.ClassModelUnavailable, 501,
			"channel %s does not implement image generation", channel)
	}
	if !g.pool.HasAccounts(channel) {
		ue := uperr.New(uperr.ClassQuotaExhausted, 429,
			"no account on channel %s; import one first", channel)
		ue.Channel = channel
		return nil, ue
	}
	out := &ImageOutcome{}
	for i := 0; i < n; i++ {
		call, err := g.generateOneImageOn(ctx, channel, ig, upstream, prompt, size, aspectRatio, watermark, refs)
		out.Attempts += call.Attempts
		if err != nil {
			return out, err
		}
		out.Images = append(out.Images, call.Images...)
		out.Account = call.Account
	}
	return out, nil
}

// generateOneImageOn is generateOneImage generalized over channel/upstream.
func (g *Gateway) generateOneImageOn(ctx context.Context, channel string, ig driver.ImageGenerator, upstreamModel, prompt, size, aspectRatio string, watermark *bool, refs []string) (*imageCall, error) {
	calls := &imageCall{}
	for attempt := 1; attempt <= maxAccountAttempts; attempt++ {
		calls.Attempts = attempt
		lease, err := g.pool.Acquire(ctx, channel, upstreamModel, "")
		if err != nil {
			return calls, g.classifyPoolError(err, channel, upstreamModel)
		}
		var res *driver.ImageResult
		var gerr error
		if full, ok := ig.(interface {
			GenerateImageFull(context.Context, *model.Account, string, string, int, string, string, *bool, []string) (*driver.ImageResult, error)
		}); ok {
			res, gerr = full.GenerateImageFull(ctx, lease.Account, upstreamModel, prompt, 1, size, aspectRatio, watermark, refs)
		} else {
			res, gerr = ig.GenerateImage(ctx, lease.Account, upstreamModel, prompt)
		}
		if gerr == nil {
			lease.Release()
			calls.Images = res.Images
			calls.Account = lease.Account
			return calls, nil
		}
		ue, ok := uperr.As(gerr)
		if ok {
			if ue.Model == "" {
				ue.Model = upstreamModel
			}
			g.pool.ApplyResult(lease.Account, upstreamModel, ue)
		}
		lease.Release()
		log.Printf("gateway: image attempt %d/%d failed on %s (%s): %v",
			attempt, maxAccountAttempts, lease.Account.Nickname, channel, gerr)
		if ctx.Err() != nil {
			return calls, gerr
		}
		if !ok || !uperr.Rotatable(ue.Class) {
			return calls, gerr
		}
	}
	return calls, uperr.New(uperr.ClassQuotaExhausted, 429,
		"every %s account failed the image request (%d attempts)", channel, maxAccountAttempts)
}

// imageModelGate returns a redirect error when the chat model is the image
// model and images are actually available; empty string means "not the gate".
func (g *Gateway) imageModelGate(modelName string) error {
	if !imageRequested(modelName) || !g.ImageGenAvailable() {
		return nil
	}
	return uperr.New(uperr.ClassBadRequest, 400,
		"model %q only generates images; use POST /v1/images/generations", model.ImageGenModel)
}
