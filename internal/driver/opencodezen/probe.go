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

// transientProbeErr reports whether a probe failure carries no information
// about the model itself. The free lane's 429 is the important one: it means
// the shared anonymous quota is spent, not that nine models died. Timeouts,
// cancellations, transport blips and edge blocks are equally untranslatable.
//
// The free-tier gate's 403 belongs here too (2026-10-18): it is answered
// network-by-network and request-by-request — the official OpenCode client
// itself draws it from some egresses while working from others — so treating
// it as "this model is gone" would empty the route table over a condition the
// model has nothing to do with (anomalyco/opencode#56334).
func transientProbeErr(err error) bool {
	ue, ok := uperr.As(err)
	if !ok {
		return false
	}
	switch ue.Class {
	case uperr.ClassUpstreamBusy, uperr.ClassUpstreamTimeout,
		uperr.ClassCanceled, uperr.ClassTransport, uperr.ClassWAF:
		return true
	case uperr.ClassBadRequest:
		return strings.Contains(ue.Message, "free-tier gate rejected")
	}
	return false
}

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
		if transientProbeErr(err) {
			c.RecordProbeTransient(modelID, ms, msg)
			return err
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
// Counts are split so a quota-limited pass does not read as "no models work":
// ok = real success, limited = transient upstream condition (route kept).
func (c *Catalog) ProbeCandidates(ctx context.Context, d *Driver, perCallTimeout time.Duration) (okCount, limitedCount, total int) {
	for _, id := range c.Candidates() {
		select {
		case <-ctx.Done():
			return okCount, limitedCount, total
		default:
		}
		total++
		pctx, cancel := context.WithTimeout(ctx, perCallTimeout)
		err := c.ProbeModel(pctx, d, id)
		if err == nil {
			okCount++
		} else if transientProbeErr(err) {
			limitedCount++
		}
		cancel()
	}
	return okCount, limitedCount, total
}
