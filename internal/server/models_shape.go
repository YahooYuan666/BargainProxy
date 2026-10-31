package server

import "bargainproxy/internal/config"

// applyModelsDevShape appends models.dev-vocabulary capability keys to a
// /v1/models entry (BP-243).
//
// The gateway's own dialect (id/object/created/owned_by/context_length/pricing)
// is preserved byte for byte — this only ADDS keys, so an existing client sees
// exactly what it saw before. The point is the other kind of client: one that
// reads the models.dev vocabulary (magpie, the OpenCode family) currently gets
// nothing from us and degrades every model to text-only, because it looks up
// `modalities`, `limit`, `tool_call` and `reasoning_options` and finds none.
// misakano7545's panel measured exactly that (commit 7490cfbf): against a
// flat-key gateway, magpie read Context/Output from context_length but left
// images and efforts empty.
//
// Rules (same three as config/modelspec.go, enforced here):
//  1. additive only — never overwrite a key already set by the caller;
//  2. single source — values come from config.ModelSpecFor, the same table the
//     panel's tier page reads, so the two cannot drift;
//  3. never invent — a model with no spec row gets no capability keys at all,
//     and cost/attachment/release_date are never written (no data source).
func applyModelsDevShape(entry map[string]any, canonical string, natives []string) {
	spec, ok := config.ModelSpecFor(canonical, natives)
	if !ok {
		return
	}
	// limit: the models.dev spelling of the two ceilings. Written only when the
	// value is known; a missing one is omitted rather than defaulted.
	limit := map[string]any{}
	if spec.ContextLen > 0 {
		limit["context"] = spec.ContextLen
	}
	if spec.MaxOutput > 0 {
		limit["output"] = spec.MaxOutput
	}
	if len(limit) > 0 {
		entry["limit"] = limit
	}
	// modalities: what the model accepts in and puts out. Output is always
	// text for a chat model — an image model never reaches this handler
	// (BP-186 routes it to /v1/images/generations).
	if len(spec.InputModes) > 0 {
		entry["modalities"] = map[string]any{
			"input":  spec.InputModes,
			"output": []string{"text"},
		}
	}
	if spec.ToolCall {
		entry["tool_call"] = true
	}
	// reasoning: true when the model can think at all. We only assert it when
	// the spec names gears or a toggle; silence means "unknown", not "false".
	if len(spec.Reasoning) > 0 || spec.ThinkingOff {
		entry["reasoning"] = true
		opts := make([]map[string]any, 0, 2)
		if len(spec.Reasoning) > 0 {
			opts = append(opts, map[string]any{"type": "effort", "values": spec.Reasoning})
		}
		if spec.ThinkingOff {
			opts = append(opts, map[string]any{"type": "toggle"})
		}
		entry["reasoning_options"] = opts
	}
	if spec.Structured {
		entry["structured_output"] = true
	}
}
