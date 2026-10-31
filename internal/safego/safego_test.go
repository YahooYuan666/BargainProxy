package safego

import (
	"sync"
	"testing"
	"time"
)

// BP-245: a panic inside a background job must not escape the goroutine.
// Without this, one nil-pointer in a parser ends the whole gateway — which is
// also the process serving the session debugging it (magpie #4553 lost a
// gateway to a stray byte in a request).
func TestGoSurvivesPanicAndKeepsSiblingsRunning(t *testing.T) {
	var wg sync.WaitGroup
	ran := make(chan struct{}, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		// The panicking job.
		done := make(chan struct{})
		go func() {
			defer close(done)
			Go("job-that-panics", func() {
				panic("boom")
			})
		}()
		<-done
	}()
	go func() {
		defer wg.Done()
		// A sibling job must still run: the panic is contained, not fatal.
		Go("job-fine", func() { ran <- struct{}{} })
	}()

	wg.Wait()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("a sibling job never ran; the panic escaped and took the process with it")
	}
	// Give the panicking goroutine time to finish so the test does not race
	// with its own recovery.
	time.Sleep(50 * time.Millisecond)
}

// Recover must also be usable inline by callers that own their goroutine.
func TestRecoverIsUsableInline(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer Recover("inline")
		panic("inline boom")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("inline Recover did not contain the panic")
	}
}
