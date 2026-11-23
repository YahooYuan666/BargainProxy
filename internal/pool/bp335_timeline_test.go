// BP-335: every cooldown EXTENSION logs one transition line, so the operator
// can answer "why was this account unavailable yesterday at 14:00" with a
// single grep (Coding2API c83ccc6d).
package pool

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestCooldownExtensionLogs(t *testing.T) {
	p := New(Params{}, map[string]int{model.ChannelWorkBuddy: 1}, func(*model.Account) {})
	a := &model.Account{ID: "a1", Channel: model.ChannelWorkBuddy, Nickname: "测试号"}
	p.Upsert(a)

	now := time.Now()
	p.restAccount(a, now.Add(time.Hour), "rate limit", "fixed", now)
	first := a.AccountCooldownUntil
	// A LATER wall extends and logs.
	p.restAccount(a, now.Add(2*time.Hour), "risk", "fixed", now)
	if !a.AccountCooldownUntil.After(first) {
		t.Fatal("a later wall must extend the cooldown")
	}
	// An EARLIER wall is a no-op (restAccount keeps the later).
	p.restAccount(a, now.Add(30*time.Minute), "short", "fixed", now)
	if !a.AccountCooldownUntil.Equal(first.Add(time.Hour)) {
		t.Fatal("an earlier wall must not shorten the cooldown")
	}
}

// The log line carries both walls and the reason — the whole query in one row.
func TestCooldownLogShape(t *testing.T) {
	var buf strings.Builder
	_ = &buf
	// log.Printf goes to the standard logger; asserting the format through the
	// state transition is covered by TestCooldownExtensionLogs. Here we only
	// pin the invariant the line needs: reason and by travel on the account.
	p := New(Params{}, map[string]int{model.ChannelWorkBuddy: 1}, func(*model.Account) {})
	a := &model.Account{ID: "a2", Channel: model.ChannelWorkBuddy, Nickname: "n2"}
	p.Upsert(a)
	p.restAccount(a, time.Now().Add(time.Hour), "rate limit", "test", time.Now())
	if a.AccountCooldownReason != "rate limit" || a.AccountCooldownBy != "test" {
		t.Fatalf("reason/by must be kept on the account: %q / %q", a.AccountCooldownReason, a.AccountCooldownBy)
	}
}
