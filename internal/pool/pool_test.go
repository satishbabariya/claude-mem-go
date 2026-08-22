package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewClampsNegativeAndZeroToOne is the regression test for a real bug
// found by hand, not anticipated: Go's own make(chan T, n) panics with
// "makechan: size out of range" for a negative n, so New(-1) — reachable
// from a mistyped or computed `-max-concurrent` flag — crashed the
// worker daemon at startup, before it ever bound its socket. Zero has a
// quieter but equally real failure mode (every Acquire would block
// forever), covered here too.
func TestNewClampsNegativeAndZeroToOne(t *testing.T) {
	for _, n := range []int{-1, -100, 0} {
		p := New(n)
		if got := p.Capacity(); got != 1 {
			t.Errorf("New(%d).Capacity() = %d, want 1 (clamped)", n, got)
		}
		// Confirm the pool is actually usable, not just reporting a
		// plausible-looking capacity — Acquire must return promptly.
		done := make(chan struct{})
		go func() {
			p.Acquire()
			p.Release()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Errorf("New(%d): Acquire did not return — pool isn't actually usable after clamping", n)
		}
	}
}

// TestMaxConcurrency verifies the pool never lets more than N holders run
// at once, using a shared counter sampled while every goroutine is inside
// its critical section — the actual property Pool exists to guarantee, not
// just "Acquire/Release don't panic."
func TestMaxConcurrency(t *testing.T) {
	const maxConcurrent = 3
	const workers = 12

	p := New(maxConcurrent)
	var current int32
	var maxObserved int32
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Acquire()
			defer p.Release()

			n := atomic.AddInt32(&current, 1)
			for {
				old := atomic.LoadInt32(&maxObserved)
				if n <= old || atomic.CompareAndSwapInt32(&maxObserved, old, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond) // hold the slot long enough for overlap to show up
			atomic.AddInt32(&current, -1)
		}()
	}
	wg.Wait()

	if maxObserved > maxConcurrent {
		t.Fatalf("observed %d concurrent holders, want at most %d", maxObserved, maxConcurrent)
	}
	if maxObserved < maxConcurrent {
		t.Fatalf("observed only %d concurrent holders, want exactly %d (pool never filled up — test isn't exercising contention)", maxObserved, maxConcurrent)
	}
}

// TestReleaseFreesASlot checks the other direction: once a holder releases,
// a new Acquire should proceed rather than deadlock waiting for a slot that
// was never actually freed.
func TestReleaseFreesASlot(t *testing.T) {
	p := New(1)
	p.Acquire()
	p.Release()

	done := make(chan struct{})
	go func() {
		p.Acquire()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Acquire did not return after the only slot was released — Release did not free it")
	}
}

func TestInFlightAndCapacityReflectRealState(t *testing.T) {
	p := New(2)
	if got := p.Capacity(); got != 2 {
		t.Fatalf("Capacity() = %d, want 2", got)
	}
	if got := p.InFlight(); got != 0 {
		t.Fatalf("InFlight() on a fresh pool = %d, want 0", got)
	}

	p.Acquire()
	if got := p.InFlight(); got != 1 {
		t.Fatalf("InFlight() after one Acquire = %d, want 1", got)
	}
	p.Acquire()
	if got := p.InFlight(); got != 2 {
		t.Fatalf("InFlight() after two Acquires = %d, want 2", got)
	}

	p.Release()
	if got := p.InFlight(); got != 1 {
		t.Fatalf("InFlight() after one Release = %d, want 1", got)
	}
	p.Release()
	if got := p.InFlight(); got != 0 {
		t.Fatalf("InFlight() after both Released = %d, want 0", got)
	}
}

func TestAcquireContextReturnsOnCancel(t *testing.T) {
	p := New(1)
	p.Acquire()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if p.AcquireContext(ctx, 5*time.Second) {
		t.Fatal("AcquireContext got a slot from a full pool")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("AcquireContext waited out the timeout instead of returning on ctx cancel")
	}
	if p.InFlight() != 1 {
		t.Fatalf("InFlight = %d after a failed acquire, want 1", p.InFlight())
	}
}

func TestAcquireContextTimesOutAndSucceeds(t *testing.T) {
	p := New(1)
	p.Acquire()
	if p.AcquireContext(context.Background(), 10*time.Millisecond) {
		t.Fatal("AcquireContext got a slot from a full pool before the timeout")
	}
	p.Release()
	if !p.AcquireContext(context.Background(), 10*time.Millisecond) {
		t.Fatal("AcquireContext failed on a pool with a free slot")
	}
}
