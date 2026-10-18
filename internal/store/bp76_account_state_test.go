package store_test

import (
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
	"testing"
	"time"
)

func TestBP76StateWriteKeepsRotatedCredentialsAndDoesNotRevive(t *testing.T) {
	st := store.NewMemory()
	now := time.Now()
	original := &model.Account{ID: "state-account", Channel: model.ChannelWorkBuddy, CredsJSON: []byte("new-credentials"), UpdatedAt: now}
	if err := st.SaveAccount(original); err != nil {
		t.Fatal(err)
	}
	state := &model.Account{ID: original.ID, Channel: original.Channel, CredsJSON: []byte("old-credentials"), Paused: true, UpdatedAt: now.Add(time.Second)}
	if err := st.SaveAccountState(state); err != nil {
		t.Fatal(err)
	}
	got, _ := st.LoadAccounts()
	if len(got) != 1 || string(got[0].CredsJSON) != "new-credentials" || !got[0].Paused {
		t.Fatalf("state persistence overwrote credentials or lost state: %+v", got)
	}
	st.SaveAccountState(&model.Account{ID: original.ID, UpdatedAt: now.Add(-time.Second)})
	got, _ = st.LoadAccounts()
	if !got[0].Paused {
		t.Fatal("stale state overwrote newer snapshot")
	}
	st.DeleteAccount(original.ID)
	if err := st.SaveAccountState(state); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LoadAccounts()
	if len(got) != 0 {
		t.Fatal("delayed state event revived deleted account")
	}
}
