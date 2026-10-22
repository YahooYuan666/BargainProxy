package pool

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// TestStickyDropsWhenCacheIsCold pins BP-95: a conversation is kept on its
// account only while the vendor's prefix cache of it is worth keeping. Staying
// buys the next turn a cache read; once the cache has gone cold, staying buys
// nothing and costs the pool its load spreading.
func TestStickyDropsWhenCacheIsCold(t *testing.T) {
	now := sinkNow
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now), SessionTTL: time.Hour},
		map[string]int{model.ChannelWorkBuddy: 4}, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	b := mkAccount("wb-b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)

	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	bound := lease.Account.ID
	p.Release(lease)

	// An answer that read a big cached prefix: the conversation is worth
	// keeping where it is.
	p.NoteSessionCache(model.ChannelWorkBuddy, "sess-1", ir.Usage{
		InputTokens: 20000, CachedInputTokens: 18000, CacheKnown: true,
	})
	if id, ok := p.SessionBinding(model.ChannelWorkBuddy, "sess-1"); !ok || id != bound {
		t.Fatalf("binding lost: %q %v", id, ok)
	}
	lease, err = p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.Account.ID != bound {
		t.Fatalf("warm cache must keep the conversation on %s, got %s", bound, lease.Account.ID)
	}
	p.Release(lease)

	// An answer that read nothing: no cache is being kept, so the binding is
	// no longer worth holding.
	p.NoteSessionCache(model.ChannelWorkBuddy, "sess-1", ir.Usage{
		InputTokens: 20000, CachedInputTokens: 0, CacheKnown: true,
	})
	p.Lock()
	worth := p.stickyWorthLocked(p.sessions[model.ChannelWorkBuddy+"|sess-1"], now)
	p.Unlock()
	if worth {
		t.Fatal("a conversation with no cache read is not worth keeping")
	}
}

// TestStickyKeptWhenChannelReportsNoCache pins the safe default: a channel that
// reports no cache accounting at all must not lose stickiness — silence is not
// evidence of a cold cache.
func TestStickyKeptWhenChannelReportsNoCache(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	p.Upsert(a)
	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.Release(lease)
	p.NoteSessionCache(model.ChannelWorkBuddy, "sess-1", ir.Usage{
		InputTokens: 20000, CacheKnown: false,
	})
	p.Lock()
	worth := p.stickyWorthLocked(p.sessions[model.ChannelWorkBuddy+"|sess-1"], sinkNow)
	p.Unlock()
	if !worth {
		t.Fatal("a channel that reports no cache accounting must keep its stickiness")
	}
}

// TestStickyDropsAfterCacheGoesCold pins the time bound: even a large cached
// prefix stops being worth a binding once the vendor would have dropped it.
func TestStickyDropsAfterCacheGoesCold(t *testing.T) {
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(sinkNow)}, nil, nil)
	a := mkAccount("wb-a", model.ChannelWorkBuddy)
	p.Upsert(a)
	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.Release(lease)
	p.NoteSessionCache(model.ChannelWorkBuddy, "sess-1", ir.Usage{
		InputTokens: 20000, CachedInputTokens: 18000, CacheKnown: true,
	})
	p.Lock()
	b := p.sessions[model.ChannelWorkBuddy+"|sess-1"]
	fresh := p.stickyWorthLocked(b, sinkNow)
	stale := p.stickyWorthLocked(b, sinkNow.Add(cacheColdAfter+time.Second))
	p.Unlock()
	if !fresh {
		t.Fatal("a warm, large cache must keep the binding")
	}
	if stale {
		t.Fatal("a cache older than the vendor's retention must not keep the binding")
	}
}

// TestColdCacheLetsAcquireRebind pins the effect on the Acquire path itself
// (not just the predicate). The default channel strategy picks the least
// recently used account, so with a warm cache the conversation must stay on
// the account that answered it, and once the cache is cold the pick must be
// free to land on the other one.
func TestColdCacheLetsAcquireRebind(t *testing.T) {
	now := sinkNow
	const ch = model.ChannelOpenAICompat
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now), SessionTTL: time.Hour},
		map[string]int{ch: 4}, nil)
	p.Upsert(mkAccount("oc-a", ch))
	p.Upsert(mkAccount("oc-b", ch))

	lease, err := p.Acquire(context.Background(), ch, "gpt-5", "sess-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	first := lease.Account.ID
	p.Release(lease)
	p.NoteSessionCache(ch, "sess-1", ir.Usage{
		InputTokens: 20000, CachedInputTokens: 18000, CacheKnown: true,
	})

	// Warm: the binding holds even though the other account is idler.
	lease2, err := p.Acquire(context.Background(), ch, "gpt-5", "sess-1")
	if err != nil {
		t.Fatalf("acquire (warm): %v", err)
	}
	if lease2.Account.ID != first {
		t.Fatalf("warm cache moved the conversation: %s -> %s", first, lease2.Account.ID)
	}
	p.Release(lease2)

	// Cold: the binding is dropped, so the least-recently-used account wins.
	p.mu.Lock()
	p.params.Now = fixedNow(now.Add(cacheColdAfter + time.Minute))
	p.mu.Unlock()
	lease3, err := p.Acquire(context.Background(), ch, "gpt-5", "sess-1")
	if err != nil {
		t.Fatalf("acquire (cold): %v", err)
	}
	defer p.Release(lease3)
	if lease3.Account.ID == first {
		t.Fatalf("cold cache still pinned the conversation to %s", first)
	}
	if id, ok := p.SessionBinding(ch, "sess-1"); !ok || id != lease3.Account.ID {
		t.Fatalf("binding = %q ok=%v, want the account just picked (%q)", id, ok, lease3.Account.ID)
	}
}
