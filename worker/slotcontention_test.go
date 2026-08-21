package worker

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"claude-mem-go/logging"
	"claude-mem-go/observer"
	"claude-mem-go/pool"
)

// TestSessionSlotContentionIsReportedAndBounded covers a failure found by
// running three real Claude Code sessions at once.
//
// A pool slot is held for a cached observer session's whole LIFETIME, not
// per observation, and sessions stay cached for sessionIdleTimeout (10
// minutes). With the default capacity of 2, a third concurrent session
// therefore waited here — with no timeout and no log line — while the
// hook reported a successful forward, the daemon accepted the connection,
// and `doctor` reported the worker reachable. Two of three sessions were
// captured; the third produced nothing, and the only evidence anywhere
// was `pool_in_flight: 2 / pool_capacity: 2` at 0% CPU.
func TestSessionSlotContentionIsReportedAndBounded(t *testing.T) {
	orig := sessionSlotWait
	sessionSlotWait = 200 * time.Millisecond
	t.Cleanup(func() { sessionSlotWait = orig })

	var logBuf bytes.Buffer
	c := newSessionCache(pool.New(1), func(ctx context.Context) (observer.Handle, error) {
		return &fakeHandle{}, nil
	})
	c.Log = logging.NewAtLevel(&logBuf, "", 0, logging.Debug)

	// First session takes the only slot and keeps it (cached).
	if _, err := c.getOrCreate(context.Background(), "sess-1"); err != nil {
		t.Fatalf("first getOrCreate: %v", err)
	}

	// Second session finds none free.
	start := time.Now()
	_, err := c.getOrCreate(context.Background(), "sess-2")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a second session got a slot from a capacity-1 pool")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("waited %s — the wait must be bounded, not indefinite", elapsed)
	}
	if !strings.Contains(err.Error(), "-max-concurrent") {
		t.Fatalf("error %q does not tell the operator how to fix it", err)
	}
	out := logBuf.String()
	if !strings.Contains(out, "waiting for an observer slot") {
		t.Fatalf("contention was not logged; it stays invisible exactly as before:\n%s", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Fatalf("contention was not logged at WARN:\n%s", out)
	}
}

// TestSessionSlotIsReusedForTheSameSession is the counterweight: the
// SAME session must never contend with itself. It is already cached, so
// it returns without touching the pool at all — if it did not, a single
// busy session would exhaust a capacity-1 pool against itself.
func TestSessionSlotIsReusedForTheSameSession(t *testing.T) {
	c := newSessionCache(pool.New(1), func(ctx context.Context) (observer.Handle, error) {
		return &fakeHandle{}, nil
	})
	if _, err := c.getOrCreate(context.Background(), "sess-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := c.getOrCreate(context.Background(), "sess-1"); err != nil {
			t.Fatalf("reuse %d failed: %v — a cached session must not re-acquire a slot", i, err)
		}
	}
	if got := c.poolInFlight(); got != 1 {
		t.Fatalf("pool in-flight = %d after five reuses of one session, want 1", got)
	}
}

// TestSessionSlotFreesOnEviction pins the recovery path: once a session is
// evicted its slot must come back, or the daemon degrades permanently
// rather than temporarily.
func TestSessionSlotFreesOnEviction(t *testing.T) {
	c := newSessionCache(pool.New(1), func(ctx context.Context) (observer.Handle, error) {
		return &fakeHandle{}, nil
	})
	if _, err := c.getOrCreate(context.Background(), "sess-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	c.evict("sess-1")
	if got := c.poolInFlight(); got != 0 {
		t.Fatalf("pool in-flight = %d after eviction, want 0 — the slot leaked", got)
	}
	if _, err := c.getOrCreate(context.Background(), "sess-2"); err != nil {
		t.Fatalf("a new session could not take the freed slot: %v", err)
	}
}
