package trae

// Diagnostic exports used by scripts/tmaxmode (2026-10-31).
//
// WHY THESE EXIST: answering "can llm_utils_chat carry a tool-bearing prompt
// past 262144 when Max Mode is asked for?" requires sending a body the driver
// itself would REFUSE to build (Stream() rejects tools+oversize before
// dispatch), while still sending it over the driver's exact header set and
// wire shape. Re-implementing the headers in a probe would measure a different
// request and settle nothing, so the driver hands out its own builders here.
//
// These are read-only: they build a body and issue one request. They touch no
// pool, no cooldown, no store, no config, and change no driver state. Nothing
// in the production path calls them.

import (
	"context"
	"io"
	"net/http"
	"strings"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// ProbeBuildSOLOBody builds the exact llm_utils_chat body this driver would
// send, then lets the caller add fields the driver normally omits (Max Mode's
// is_max_mode / prompt_max_tokens). It reuses buildSOLOBody verbatim so the
// message encoding, config_name routing and tool shapes are the real ones.
func ProbeBuildSOLOBody(call driver.ChatCall, function string, extra map[string]any) ([]byte, error) {
	raw, err := buildSOLOBody(call, function)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return raw, nil
	}
	// Patch by round-trip only to add top-level keys; the driver's own bytes
	// are otherwise untouched.
	return mergeTopLevel(raw, extra)
}

// ProbePostSOLO sends raw to llm_utils_chat with the driver's real SOLO header
// set and returns the HTTP status plus the first 64KiB of the body. The caller
// interprets it; this function never classifies.
func ProbePostSOLO(ctx context.Context, d *Driver, creds *Creds, raw []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.agentBase()+"/api/agent/v3/llm_utils_chat", strings.NewReader(string(raw)))
	if err != nil {
		return 0, "", err
	}
	soloHeaders(req, creds, true)
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(body), nil
}

// ProbeEstimator exposes the driver's own prompt-size estimate so a probe can
// report the same number the dispatch decision would use.
func ProbeEstimator(req *ir.ChatRequest) int { return estimateInputTokens(req) }
