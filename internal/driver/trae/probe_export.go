package trae

// Diagnostic exports used by scripts/tmaxmode (2026-10-31).
//
// WHY THESE EXIST: settling "how big a prompt does llm_utils_chat really carry?"
// needs a body built over the driver's exact header set and wire shape, with
// extra fields (Max Mode's is_max_mode / prompt_max_tokens) the driver never
// sends itself. Re-implementing the headers in a probe would measure a
// different request and settle nothing, so the driver hands out its own
// builders here.
//
// These are read-only: they build a body and issue one request. They touch no
// pool, no cooldown, no store, no config, and change no driver state. Nothing
// in the production path calls them.

import (
	"context"
	"fmt"
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

// ProbeEstimator exposes a coarse prompt-size estimate so a probe can print
// "we thought it was N tokens" beside "the upstream said M" — the comparison
// that corrected the 262144 myth on 2026-11-01.
//
// DIAGNOSTIC ONLY. No dispatch decision uses it: the driver no longer gates on
// a local estimate at all (the two-surface split it served was removed — see
// the comment in Stream).
//
// It counts what buildSOLOBody actually puts on the wire: content text, image
// tiles (never their base64 length), tool-call arguments, and the tool
// declarations. History THINKING is deliberately excluded — this driver never
// sends it (buildSOLOBody walks m.Content only), and counting it inflated a
// real 2026-11-23 request 2.85× (393434 tokens read as 1120971).
func ProbeEstimator(req *ir.ChatRequest) int {
	if req == nil {
		return 0
	}
	chars, images := 0, 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			switch p.Kind {
			case "text":
				chars += len(p.Text)
			case "image":
				images++
			}
		}
		for _, tc := range m.ToolCalls {
			chars += len(tc.Arguments)
		}
	}
	tools := 0
	for _, td := range req.Tools {
		tools += len(td.Name) + len(td.Description)
		if td.Parameters != nil {
			tools += len(fmt.Sprint(td.Parameters))
		}
	}
	return (chars+tools)/3 + images*768
}
