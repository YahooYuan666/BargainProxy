package panel

import (
	"testing"

	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/model"
)

// TestMatchExistingFindsTheSamePersonByAltID pins the fix for a real
// double-registration (2026-10-24).
//
// The official AutoClaw client records one person under two id systems: a
// UUID-shaped accountId and a numeric userId. The panel's SMS login stores the
// UUID as the account uid, while the client's JWT carries the number. An import
// that matched on the primary id alone therefore registered a SECOND row for an
// account the panel already had — two AutoClaw entries, one of them dead.
func TestMatchExistingFindsTheSamePersonByAltID(t *testing.T) {
	// The account the panel already had (SMS login), keyed by its UUID id.
	existing := &model.Account{
		ID:       "autoclaw:153f2fb13e26f880c2d08e1cf98f445a",
		Channel:  model.ChannelAutoClaw,
		UID:      "153f2fb13e26f880c2d08e1cf98f445a",
		Nickname: "18191367452",
	}
	known := map[string]*model.Account{
		existing.ID:                existing,
		"autoclaw:" + existing.UID: existing,
	}

	// What the importer produces for that same person: the numeric id as
	// primary, the UUID as an alternate.
	imported := &model.Account{
		ID:      "autoclaw:880098",
		Channel: model.ChannelAutoClaw,
		UID:     "880098",
	}
	ia := autoclaw.ImportedAccount{
		Account: imported,
		Realm:   "cn",
		AltIDs:  []string{"153f2fb13e26f880c2d08e1cf98f445a"},
	}

	got := matchExisting(known, imported, ia)
	if got == nil {
		t.Fatal("the same person must be recognised through the alternate id, not registered twice")
	}
	if got.ID != existing.ID {
		t.Fatalf("matched %q, want the existing row %q", got.ID, existing.ID)
	}
}

// TestMatchExistingLeavesStrangersAlone: a genuinely new account must not be
// folded into somebody else's row.
func TestMatchExistingLeavesStrangersAlone(t *testing.T) {
	known := map[string]*model.Account{
		"autoclaw:182537": {ID: "autoclaw:182537", Channel: model.ChannelAutoClaw, UID: "182537"},
	}
	imported := &model.Account{ID: "autoclaw:999999", Channel: model.ChannelAutoClaw, UID: "999999"}
	ia := autoclaw.ImportedAccount{Account: imported, Realm: "cn", AltIDs: []string{"888888"}}

	if got := matchExisting(known, imported, ia); got != nil {
		t.Fatalf("a new account was matched to %q", got.ID)
	}
}

// TestAltIDsOfPrefixesChannel: alternate ids are pool keys, so they must carry
// the channel prefix or the lookup can never hit.
func TestAltIDsOfPrefixesChannel(t *testing.T) {
	got := altIDsOf(autoclaw.ImportedAccount{AltIDs: []string{"abc", "", "def"}})
	if len(got) != 2 {
		t.Fatalf("got %v, want two non-empty keys", got)
	}
	for _, k := range got {
		if k != model.ChannelAutoClaw+":abc" && k != model.ChannelAutoClaw+":def" {
			t.Fatalf("unprefixed key %q", k)
		}
	}
}
