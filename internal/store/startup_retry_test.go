package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A server that is still replaying crash recovery answers 57P03. That is the
// only error the boot is allowed to wait out: the port is already open, so the
// launcher has moved on, and giving up here exits the gateway before it ever
// listens (observed 2026-10-22 19:25, three starts in a row).
func TestStartingUpIsTheRecoveryStateOnly(t *testing.T) {
	if !startingUp(&pgconn.PgError{Code: "57P03"}) {
		t.Fatal("57P03 must be treated as still starting")
	}
	if startingUp(&pgconn.PgError{Code: "28P01"}) {
		t.Fatal("an authentication failure must not be retried")
	}
	if startingUp(errors.New("connection refused")) {
		t.Fatal("a wrapped non-postgres error must not be retried")
	}
}

// Once the deadline passes, the wait has to stop even if the server keeps
// saying it is starting. Otherwise a database that never finishes recovery
// hangs the boot forever.
func TestSleepGivesUpWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, 30*time.Second) {
		t.Fatal("a cancelled context must not wait")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("gave up too slowly: %s", time.Since(start))
	}
}
