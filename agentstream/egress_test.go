package agentstream

import (
	"sync"
	"testing"
	"time"
)

// Goal 31, the-starvation-answer, decision 3: data frames must not be able to starve control
// frames. These test the yield primitive directly, because the send itself needs a live stream.

func TestWaitForControlIdle_ReturnsImmediatelyWhenNoControlIsWaiting(t *testing.T) {
	controlWaiting.Store(0)
	start := time.Now()
	if waitForControlIdle() {
		t.Fatal("bulk yielded with no control waiting; a quiet node must not slow a terminal")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Millisecond {
		t.Fatalf("bulk paused %v on an idle node; it must not pause at all", elapsed)
	}
}

func TestWaitForControlIdle_YieldsWhileControlIsWaiting(t *testing.T) {
	controlWaiting.Store(1)
	var yielded bool
	done := make(chan struct{})
	go func() {
		yielded = waitForControlIdle()
		close(done)
	}()

	// Still yielding a few ticks later: it has not raced past the control sender.
	time.Sleep(10 * bulkYield)
	select {
	case <-done:
		t.Fatal("bulk returned while control was still waiting")
	default:
	}

	controlWaiting.Store(0)
	select {
	case <-done:
	case <-time.After(2 * bulkMaxYield):
		t.Fatal("bulk never resumed after control drained")
	}
	if !yielded {
		t.Fatal("bulk did not report that it gave way")
	}
}

// The ceiling matters as much as the yield: a node under sustained control load must not stall a
// terminal for ever. A console that never prints is as broken as one that starves the control
// plane.
func TestWaitForControlIdle_GivesUpAtTheCeiling(t *testing.T) {
	controlWaiting.Store(1)
	defer controlWaiting.Store(0)

	start := time.Now()
	waitForControlIdle()
	elapsed := time.Since(start)

	if elapsed < bulkMaxYield {
		t.Fatalf("gave up after %v, before the %v ceiling", elapsed, bulkMaxYield)
	}
	if elapsed > 3*bulkMaxYield {
		t.Fatalf("waited %v, far past the %v ceiling", elapsed, bulkMaxYield)
	}
}

// Concurrent control senders must all be counted, or one finishing would let bulk through while
// the others are still waiting.
func TestControlWaiting_CountsConcurrentSenders(t *testing.T) {
	controlWaiting.Store(0)
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			controlWaiting.Add(1)
			<-release
			controlWaiting.Add(-1)
		}()
	}
	// Let them all register.
	for controlWaiting.Load() < 5 {
		time.Sleep(time.Millisecond)
	}
	if got := controlWaiting.Load(); got != 5 {
		t.Fatalf("counted %d concurrent control senders, want 5", got)
	}
	close(release)
	wg.Wait()
	if got := controlWaiting.Load(); got != 0 {
		t.Fatalf("count left at %d, want 0", got)
	}
}
