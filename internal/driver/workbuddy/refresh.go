package workbuddy

import (
	"context"
	"fmt"
	"time"

	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

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
		d.refreshMu.Lock()
		f.creds, f.err, f.finished = updated, err, time.Now()
		if err == nil {
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
