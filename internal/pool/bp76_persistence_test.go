package pool_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
)

func TestBP76SlowPersistenceDoesNotBlockOtherChannelAndContext(t *testing.T) {
	var callbacks atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2, model.ChannelQoder: 1}, func(*model.Account) {
		if callbacks.Add(1) == 1 {
			close(entered)
			<-release
		}
	})
	p.Upsert(&model.Account{ID: "audit-wb", Channel: model.ChannelWorkBuddy})
	p.Upsert(&model.Account{ID: "audit-qoder", Channel: model.ChannelQoder})
	firstDone := make(chan error, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "audit-model", "")
		if lease != nil {
			lease.Release()
		}
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("persistence callback was not reached")
	}
	secondDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	go func() {
		lease, err := p.Acquire(ctx, model.ChannelQoder, "audit-model", "")
		if lease != nil {
			lease.Release()
		}
		secondDone <- err
	}()
	blocked := false
	select {
	case <-secondDone:
	case <-time.After(150 * time.Millisecond):
		blocked = true
	}
	close(release)
	<-firstDone
	if blocked {
		<-secondDone
		t.Fatal("slow persistence during one channel lease blocked unrelated channel Acquire beyond its context deadline; want lock-free I/O or bounded cancellation")
	}
}
