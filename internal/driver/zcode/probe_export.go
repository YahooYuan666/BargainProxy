package zcode

// Diagnostic export used by scripts/zraw (2026-11-01, BP-253).
//
// WHY: every zcode leg was dying in ~40ms with "stream ended without
// message_stop" — a 200 whose body the parser could not use. The pool then
// cools the channel, so the failure cannot be re-observed from the gateway.
// This sends ONE request over the driver's own body builder and header set and
// returns the upstream's raw opening bytes, so the cause is answered by the
// bytes rather than guessed.
//
// Read-only: builds a body, issues one request, returns what came back. It
// touches no pool, no cooldown, no store, and changes no driver state.

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"bargainproxy/internal/driver"
)

// ProbeRaw posts one request through the driver's real path and returns the
// HTTP status plus the first maxHead bytes of the response body.
func ProbeRaw(ctx context.Context, d *Driver, call driver.ChatCall, maxHead int) (int, string, error) {
	creds, err := ParseCreds(call.Account.CredsJSON)
	if err != nil {
		return 0, "", err
	}
	body, _, _, err := d.normalizeBody(call, creds, true)
	if err != nil {
		return 0, "", err
	}
	sessionID := sessionIDFor(creds, accountIDOf(call), call.Request.SessionKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base()+messagesPath,
		bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	identityHeaders(req, creds, defaultAppVersion, sessionID)
	// No totals timeout: the caller bounds it through ctx.
	client := &http.Client{Transport: d.http.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if maxHead <= 0 {
		maxHead = 4096
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxHead)))
	return resp.StatusCode, string(head), nil
}
