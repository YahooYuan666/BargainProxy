package workbuddy

import (
	"context"
	"fmt"
	"log"
	"time"

	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// accessTokenSlack is how far ahead of the expiry clock an access token is
// considered spent. Upstream chat answers 401 the moment the token is past
// its exp, so anything strictly in the future still works; the slack only
// avoids claiming "usable" for a token about to lapse mid-request.
const accessTokenSlack = time.Minute

// accessTokenUsable reports whether the access token in these credentials has
// not expired yet. Unknown expiry (0) is NOT usable: BP-145 must never turn a
// genuinely dead account into a permanent zombie.
func (c *Creds) accessTokenUsable(now time.Time) bool {
	if c.AccessToken == "" || c.ExpiresAt <= 0 {
		return false
	}
	return time.Unix(c.ExpiresAt, 0).After(now.Add(accessTokenSlack))
}

// isCredentialRejection reports whether the refresh failed because the
// CREDENTIAL was refused (the upstream said no), as opposed to failing because
// the upstream or the network was unavailable. Only the former means "this
// token is dead but the account may be fine"; a 503 or a dropped socket says
// nothing about the credential and must keep failing loudly (BP-76).
func isCredentialRejection(err error) bool {
	ue, ok := uperr.As(err)
	if !ok {
		return false
	}
	if ue.Class != uperr.ClassAuth {
		return false
	}
	// 0 means the request never reached an answer — transport, not a verdict.
	return ue.Status == 401 || ue.Status == 403
}

type refreshFlight struct {
	done     chan struct{}
	oldToken string
	creds    []byte
	err      error
	finished time.Time
}

func (d *Driver) refresh(ctx context.Context, acc *model.Account, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.refreshMu.Lock()
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		d.refreshMu.Unlock()
		return err
	}
	if !force && (creds.ExpiresAt == 0 || time.Until(time.Unix(creds.ExpiresAt, 0)) > 10*time.Minute) {
		d.refreshMu.Unlock()
		return nil
	}
	// BP-144: a key account has nothing to renew. Calling the refresh route
	// with one spends an RTT and turns a working account into a ClassAuth
	// cooldown; the key either works or the user makes a new one.
	if creds.IsAPIKey() {
		d.refreshMu.Unlock()
		return uperr.New(uperr.ClassAuth, 401, "workbuddy: an API key account cannot refresh; check the key in the panel")
	}
	if creds.RefreshToken == "" {
		d.refreshMu.Unlock()
		return uperr.New(uperr.ClassAuth, 401, "workbuddy refresh: no refresh token")
	}
	key := creds.RealmOrDefault() + "|" + creds.UID
	if creds.UID == "" {
		key = creds.RealmOrDefault() + "|" + acc.ID
	}
	if creds.UID == "" && acc.ID == "" {
		d.refreshMu.Unlock()
		return fmt.Errorf("workbuddy refresh: missing account identity")
	}
	if d.refreshes == nil {
		d.refreshes = map[string]*refreshFlight{}
	}
	for k, f := range d.refreshes {
		if !f.finished.IsZero() && time.Since(f.finished) > 10*time.Minute {
			delete(d.refreshes, k)
		}
	}
	if f := d.refreshes[key]; f != nil {
		if f.finished.IsZero() {
			d.refreshMu.Unlock()
			return d.awaitRefresh(ctx, acc, f)
		}
		newer, _ := ParseCreds(f.creds)
		if f.err == nil && newer != nil && newer.RefreshToken != creds.RefreshToken && f.oldToken == creds.RefreshToken {
			acc.SetCredentials(f.creds)
			d.refreshMu.Unlock()
			return nil
		}
	}
	f := &refreshFlight{done: make(chan struct{}), oldToken: creds.RefreshToken}
	d.refreshes[key] = f
	d.refreshMu.Unlock()
	go func() {
		rpcCtx, cancel := context.WithTimeout(context.Background(), httpx.RPCTimeout())
		defer cancel()
		updated, err := d.refreshOnce(rpcCtx, creds)
		// BP-145: a dead REFRESH token is not a dead ACCOUNT. Upstream
		// sometimes answers 12153 "Session doesn't have required client" to
		// every refresh while the access token in hand is still good for
		// weeks — misakano measured 4 of 29 accounts like that (issue #83):
		// AT with 26-333 days left, /v2/chat/completions answering 200, while
		// the old code counted each one as session-dead and disabled the
		// account after three keepalive rounds.
		//
		// The judge is the access token's own clock, nothing else: not the
		// upstream's wording (the same failure alternates between "Session
		// doesn't have required client" and "Invalid refresh token" on retry)
		// and not the JWT's azp (all 29 of those accounts carried "console").
		//
		// Only CREDENTIAL rejections qualify. A 503 / transport failure from
		// the refresh route is a transient upstream problem: swallowing it
		// would tell the caller the renewal succeeded when nothing is known,
		// and hide the outage from the refresh task (BP-76: a failure must
		// never be cached as a success).
		if err != nil && isCredentialRejection(err) && creds.accessTokenUsable(time.Now()) {
			log.Printf("workbuddy: token refresh refused (%v) but the access token is still valid — keeping the account in service", err)
			updated, err = acc.Credentials(), nil
		}
		d.refreshMu.Lock()
		f.creds, f.err, f.finished = updated, err, time.Now()
		if err == nil && updated != nil {
			acc.SetCredentials(updated)
		} else {
			delete(d.refreshes, key)
		}
		close(f.done)
		d.refreshMu.Unlock()
	}()
	return d.awaitRefresh(ctx, acc, f)
}

func (d *Driver) awaitRefresh(ctx context.Context, acc *model.Account, f *refreshFlight) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
	}
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if f.err != nil {
		return f.err
	}
	if string(acc.Credentials()) != string(f.creds) {
		acc.SetCredentials(f.creds)
	}
	return nil
}
