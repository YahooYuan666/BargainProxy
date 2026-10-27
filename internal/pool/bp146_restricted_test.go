package pool

// BP-146: one restricted account must not cost the rest of the pool.
//
// CodeBuddy's 403/11140 is a per-account blacklist (per egress IP when several
// accounts share one). One request used to walk three accounts of the same
// channel — Coding2API measured it: three accounts hit at 08:21, the same
// round again at 08:38, and the account touched at 08:41 was banned three
// minutes later (f428e33). Nothing about this request is wrong; the pattern is.
//
// Two guards: (1) `uperr.Rotatable` refuses another account of the same
// channel within one request, and (2) a second restriction on the same
// channel inside a short window pauses the channel for a while, so the next
// request does not walk the remaining accounts either.

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestRestrictedIsNotRotatable is the class-level rule the gateway reads.
func TestRestrictedIsNotRotatable(t *testing.T) {
	if uperr.Rotatable(uperr.ClassAccountRestricted) {
		t.Fatalf("an upstream-restricted account must not rotate within its channel")
	}
	// Sanity: the classes that legitimately rotate must keep doing so.
	for _, c := range []string{uperr.ClassSoftRate, uperr.ClassModelRate, uperr.ClassAuth, uperr.ClassInternal} {
		if !uperr.Rotatable(c) {
			t.Errorf("%s should still rotate to another account", c)
		}
	}
}

func restrictedPool(t *testing.T) (*Pool, *[]*model.Account) {
	t.Helper()
	now := time.Now()
	var accts []*model.Account
	p := New(Params{
		MaxQueueWait: time.Second,
		SessionTTL:   time.Minute,
		CostTTL:      time.Minute,
		Now:          func() time.Time { return now },
	}, nil, nil)
	for _, id := range []string{"wb-a", "wb-b", "wb-c"} {
		a := &model.Account{ID: id, Channel: model.ChannelWorkBuddy, Nickname: id}
		p.Upsert(a)
		accts = append(accts, a)
	}
	return p, &accts
}

// TestOneRestrictionPausesNothing reorders nothing: a single ban disables the
// account and leaves the channel selectable (someone may still be healthy).
func TestOneRestrictionPausesNothing(t *testing.T) {
	p, accts := restrictedPool(t)
	a := (*accts)[0]
	p.ApplyResult(a, "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140 request illegal"))

	if !a.Disabled {
		t.Fatalf("the restricted account must be disabled")
	}
	if _, paused := p.ChannelBusyUntil(model.ChannelWorkBuddy, "glm-5.3", time.Now()); paused {
		t.Fatalf("one restriction must not pause the whole channel")
	}
}

// TestSecondRestrictionPausesTheChannel is the escalation: two bans on one
// channel in a short window means the pattern is the trigger.
func TestSecondRestrictionPausesTheChannel(t *testing.T) {
	p, accts := restrictedPool(t)
	p.ApplyResult((*accts)[0], "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140 request illegal"))
	if busy, _ := p.ChannelBusy(model.ChannelWorkBuddy); busy {
		t.Fatalf("channel paused after the FIRST restriction")
	}
	p.ApplyResult((*accts)[1], "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140 request illegal"))

	if _, paused := p.ChannelBusyUntil(model.ChannelWorkBuddy, "glm-5.3", time.Now()); !paused {
		t.Fatalf("a second restriction in the window must pause the channel")
	}
	// The third account must not be reachable for a new request meanwhile.
	if ok, _ := p.NextAvailable(model.ChannelWorkBuddy, "glm-5.3"); ok {
		t.Fatalf("the channel still reports an available account after escalation")
	}
}

// TestRestrictionWindowExpires: the escalation is a window, not a ratchet —
// two bans an hour apart are two independent accidents.
func TestRestrictionWindowExpires(t *testing.T) {
	now := time.Now()
	var need []*model.Account
	p := New(Params{
		MaxQueueWait: time.Second,
		SessionTTL:   time.Minute,
		CostTTL:      time.Minute,
		Now:          func() time.Time { return now },
	}, nil, nil)
	for _, id := range []string{"wb-a", "wb-b"} {
		a := &model.Account{ID: id, Channel: model.ChannelWorkBuddy, Nickname: id}
		p.Upsert(a)
		need = append(need, a)
	}
	p.ApplyResult(need[0], "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140"))
	now = now.Add(restrictedChannelWindow + time.Minute) // window over
	p.ApplyResult(need[1], "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140"))

	if _, paused := p.ChannelBusyUntil(model.ChannelWorkBuddy, "glm-5.3", now); paused {
		t.Fatalf("restrictions an hour apart must not pause the channel")
	}
}

// TestRestrictionEscalationIsPerChannel: a ban on one channel says nothing
// about another upstream.
func TestRestrictionEscalationIsPerChannel(t *testing.T) {
	p, accts := restrictedPool(t)
	trae := &model.Account{ID: "trae-a", Channel: model.ChannelTrae, Nickname: "trae-a"}
	p.Upsert(trae)

	p.ApplyResult((*accts)[0], "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140"))
	p.ApplyResult((*accts)[1], "glm-5.3", uperr.New(uperr.ClassAccountRestricted, 403, "11140"))

	if _, paused := p.ChannelBusyUntil(model.ChannelTrae, "glm-5.3", time.Now()); paused {
		t.Fatalf("another channel was paused by workbuddy restrictions")
	}
}
