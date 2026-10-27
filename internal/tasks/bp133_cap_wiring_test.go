package tasks

import (
	"context"
	"sort"
	"testing"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/store"
)

// Wiring-level BP-133: with the cap set, the night-owl fan-out runs only the
// top-N accounts by credit balance (the helper test covers the selection;
// this one covers that runPerAccount actually applies it).
func TestBlackcatCapWiring(t *testing.T) {
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{}, func(a *model.Account) { _ = st.SaveAccount(a) })
	for _, id := range []string{"a", "b", "c"} {
		p.Upsert(&model.Account{ID: id, Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	}
	// Credits ride on the pool's copies.
	accounts := p.Accounts(model.ChannelWorkBuddy)
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	accounts[0].Credits = model.Credits{Remain: 100} // a
	accounts[1].Credits = model.Credits{Remain: 900} // b
	accounts[2].Credits = model.Credits{Remain: 500} // c

	s := New(&gateway.Gateway{}, st, p, nil)
	s.blackcatCap = 2
	s.accountSpread = 0 // the test must not sleep out the 5-minute spread window
	var ran []string
	_, err := s.runPerAccount(context.Background(), TaskBlackcatWorkBuddy, "", model.ChannelWorkBuddy,
		func(ctx context.Context, acc *model.Account) (string, error) {
			ran = append(ran, acc.ID)
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 {
		t.Fatalf("ran %v, want exactly the top-2 by credits", ran)
	}
	for _, id := range ran {
		if id == "a" { // 100 credits, the odd one out
			t.Fatalf("low-credit account %q ran despite cap", id)
		}
	}
}
