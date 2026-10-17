// Probe: one minimal live chat call per candidate model — the only honest
// answer to "is this free model actually usable right now" (the live list
// says "listed", metadata says "free"; neither says "works"). Runs through
// the driver's own chat path so a pass proves the whole chain: disguise,
// gate tools, streaming.
package opencodezen

import (
	"context"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// ProbeModel performs one minimal gated chat call against modelID and records
// the outcome into the catalog. Any successful stream completion counts —
// content is discarded.
func (c *Catalog) ProbeModel(ctx context.Context, d *Driver, modelID string) error {
	acc := &model.Account{
		ID:        model.ChannelOpenCodeZen + ":probe",
		Channel:   model.ChannelOpenCodeZen,
		CredsJSON: []byte(`{"base_url":"` + c.base + `"}`),
	}
	req := &ir.ChatRequest{
		Model: modelID,
		// Generous on purpose: reasoning-heavy models (muse-spark on the
		// responses lane) spend the whole budget on thinking and finish
		// "incomplete" without text when this is small.
		MaxTokens: 512,
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "只回答两个字：收到"}},
		}},
		Stream: true,
	}
	start := time.Now()
	resp, err := d.Stream(ctx, driver.ChatCall{
		Account:       acc,
		UpstreamModel: modelID,
		Request:       req,
	}, func(ev ir.StreamEvent) error { return nil })
	ms := time.Since(start).Milliseconds()
	if err != nil {
		msg := "unknown error"
		if ue, ok := uperr.As(err); ok {
			msg = ue.Error()
		} else if err != nil {
			msg = err.Error()
		}
		c.RecordProbe(modelID, false, ms, msg)
		return err
	}
	// A 200 with nothing in it is not "usable": reasoning-heavy models used to
	// burn a tiny budget on thinking and return nothing (muse-spark*).
	var text strings.Builder
	for _, p := range resp.Message.Content {
		text.WriteString(p.Text)
	}
	if strings.TrimSpace(text.String()) == "" && len(resp.Message.ToolCalls) == 0 {
		msg := "upstream returned an empty completion"
		c.RecordProbe(modelID, false, ms, msg)
		return uperr.New(uperr.ClassInternal, 200, "opencodezen: %s", msg)
	}
	c.RecordProbe(modelID, true, ms, "")
	return nil
}

// ProbeCandidates probes every candidate model sequentially (the free lane
// shares one anonymous quota; parallelism would just rate-limit ourselves).
// Returns the number of OK probes.
func (c *Catalog) ProbeCandidates(ctx context.Context, d *Driver, perCallTimeout time.Duration) (okCount, total int) {
	for _, id := range c.Candidates() {
		select {
		case <-ctx.Done():
			return okCount, total
		default:
		}
		total++
		pctx, cancel := context.WithTimeout(ctx, perCallTimeout)
		if err := c.ProbeModel(pctx, d, id); err == nil {
			okCount++
		}
		cancel()
	}
	return okCount, total
}
