package pool

import (
	"testing"
	"time"
)

func TestTryAcquireDoesNotBlock(t *testing.T) {
	p := New(1)
	if !p.TryAcquire() {
		t.Fatal("TryAcquire failed on an empty pool")
	}
	if p.TryAcquire() {
		t.Fatal("TryAcquire succeeded on a full pool — it must report contention, not wait")
	}
	p.Release()
	if !p.TryAcquire() {
		t.Fatal("TryAcquire failed after a Release")
	}
}

// TestAcquireWithinBoundsTheWait covers the hazard the plain Acquire is:
// a slot here is held for a cached observer session's whole lifetime, so
// with the default capacity of 2 a third concurrent Claude Code session
// blocked indefinitely — silently, with every health check still green.
func TestAcquireWithinBoundsTheWait(t *testing.T) {
	p := New(1)
	p.Acquire()

	start := time.Now()
	if p.AcquireWithin(150 * time.Millisecond) {
		t.Fatal("AcquireWithin succeeded against a full pool")
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned after %s, want it to have actually waited", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("waited %s for a 150ms deadline — the bound is not being honored", elapsed)
	}
}

// TestAcquireWithinSucceedsWhenASlotFrees pins that the bound is a
// deadline, not a rejection: an event arriving during a busy moment
// should still be captured once a slot frees.
func TestAcquireWithinSucceedsWhenASlotFrees(t *testing.T) {
	p := New(1)
	p.Acquire()
	go func() { time.Sleep(80 * time.Millisecond); p.Release() }()

	if !p.AcquireWithin(5 * time.Second) {
		t.Fatal("AcquireWithin gave up even though a slot freed well inside the deadline")
	}
}
