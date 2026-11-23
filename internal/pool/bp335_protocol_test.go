// BP-335: ClassProtocol must charge no account penalty, and ClassEmptyReply
// must not either (an upstream that answered nothing says nothing about the
// account). Both used to land in ClassInternal's default branch, which feeds
// ConsecutiveFails.
package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func protocolPool(t *testing.T) (*Pool, *model.Account) {
	t.Helper()
	p := New(Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 1}, func(*model.Account) {})
	a := &model.Account{ID: "a1", Channel: model.ChannelWorkBuddy}
	p.Upsert(a)
	return p, a
}

func TestProtocolClassChargesNoPenalty(t *testing.T) {
	p, a := protocolPool(t)
	before := a.ConsecutiveFails
	p.ApplyResult(a, "m", uperr.New(uperr.ClassProtocol, 500, "bad body"))
	if a.ConsecutiveFails != before {
		t.Fatalf("ClassProtocol must not feed ConsecutiveFails: %d → %d", before, a.ConsecutiveFails)
	}
}

func TestEmptyReplyClassChargesNoPenalty(t *testing.T) {
	p, a := protocolPool(t)
	before := a.ConsecutiveFails
	p.ApplyResult(a, "m", uperr.New(uperr.ClassEmptyReply, 0, "nothing"))
	if a.ConsecutiveFails != before {
		t.Fatalf("ClassEmptyReply must not feed ConsecutiveFails: %d → %d", before, a.ConsecutiveFails)
	}
}

// The old behaviour: ClassInternal DOES charge — this asserts the contrast so a
// future refactor cannot quietly route the new classes back through it.
func TestInternalStillCharges(t *testing.T) {
	p, a := protocolPool(t)
	before := a.ConsecutiveFails
	p.ApplyResult(a, "m", uperr.New(uperr.ClassInternal, 500, "unclassified"))
	if a.ConsecutiveFails <= before {
		t.Fatalf("ClassInternal must still charge a strike (contrast case): %d → %d", before, a.ConsecutiveFails)
	}
}
