package antigravity

// Diagnostic export used by scripts/agraw (2026-11-01, BP-256).
//
// WHY: gemini requests hung for minutes with "http2: timeout awaiting response
// headers" and were eventually canceled by the client, so nothing in the
// gateway log said WHICH host was hanging or whether the other one worked. The
// driver only fails over on transport errors and 5xx — a HANG is neither, so
// the fallback host was never reached (and when the client's context is
// finally canceled, the fallback runs with that same dead context and cannot
// succeed either).
//
// This sends the driver's own body to each host in turn with a per-host
// deadline and reports what each one actually did. Read-only: no pool, no
// cooldown, no store, no dump.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
)

// ProbeResult is one host's answer.
type ProbeResult struct {
	Host     string
	Status   int
	Elapsed  time.Duration
	Head     string
	TimedOut bool
	Err      error
}

// ProbeHosts posts the same body to every configured host with its own
// deadline (perHost), so a hanging primary is distinguishable from a healthy
// fallback.
func ProbeHosts(ctx context.Context, d *Driver, call driver.ChatCall, perHost time.Duration, maxHead int) ([]ProbeResult, error) {
	creds, project, err := d.resolveCreds(ctx, call.Account)
	if err != nil {
		return nil, err
	}
	lookup, _ := d.sigLookup(call.UpstreamModel, call.Account.ID)
	body, err := buildRequest(project, call.UpstreamModel, call.Request, lookup)
	if err != nil {
		return nil, err
	}
	hosts := []string{d.cfg.DailyURL, d.cfg.BaseURL}
	var out []ProbeResult
	for _, h := range hosts {
		if h == "" {
			continue
		}
		res := ProbeResult{Host: h}
		hctx, cancel := context.WithTimeout(ctx, perHost)
		req, rerr := http.NewRequestWithContext(hctx, http.MethodPost,
			strings.TrimRight(h, "/")+"/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(body))
		if rerr != nil {
			cancel()
			res.Err = rerr
			out = append(out, res)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		req.Header.Set("User-Agent", uaForModel(call.UpstreamModel))
		req.Header.Set("Accept", "text/event-stream")
		start := time.Now()
		resp, derr := d.http.Do(req)
		res.Elapsed = time.Since(start).Round(time.Millisecond)
		if derr != nil {
			res.Err = derr
			res.TimedOut = hctx.Err() != nil
			cancel()
			out = append(out, res)
			continue
		}
		if maxHead <= 0 {
			maxHead = 1024
		}
		head, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxHead)))
		resp.Body.Close()
		res.Status = resp.StatusCode
		res.Head = string(head)
		cancel()
		out = append(out, res)
	}
	return out, nil
}

// Describe renders one result for a log line.
func (r ProbeResult) Describe() string {
	switch {
	case r.Err != nil && r.TimedOut:
		return fmt.Sprintf("%s: TIMEOUT after %s (no response headers)", r.Host, r.Elapsed)
	case r.Err != nil:
		return fmt.Sprintf("%s: transport error after %s: %v", r.Host, r.Elapsed, r.Err)
	default:
		h := strings.ReplaceAll(strings.TrimSpace(r.Head), "\n", " | ")
		if len(h) > 300 {
			h = h[:300] + "…"
		}
		return fmt.Sprintf("%s: HTTP %d after %s — %s", r.Host, r.Status, r.Elapsed, h)
	}
}
