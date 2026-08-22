package worker

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/pool"
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

	// First session takes the only slot. Hold its turn lock: a session
	// with a turn in flight is the one thing getOrCreate cannot evict to
	// make room, so this is the only way a capacity-1 pool is genuinely
	// full — an idle cached session would be evicted instead (see
	// TestSessionSlotEvictsIdleLRUInsteadOfDropping).
	first, err := c.getOrCreate(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("first getOrCreate: %v", err)
	}
	first.mu.Lock()
	defer first.mu.Unlock()

	// Second session finds none free.
	start := time.Now()
	_, err = c.getOrCreate(context.Background(), "sess-2")
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

// TestSessionSlotEvictsIdleLRUInsteadOfDropping pins the fix for a real
// end-to-end failure: with the default capacity of 2, eight new sessions
// arriving while one idle session was cached got one slot between them;
// the other seven waited the full sessionSlotWait and their events were
// dropped. An idle cached session must give up its slot to an arriving
// session, oldest first.
func TestSessionSlotEvictsIdleLRUInsteadOfDropping(t *testing.T) {
	orig := sessionSlotWait
	sessionSlotWait = 5 * time.Second // must not be reached
	t.Cleanup(func() { sessionSlotWait = orig })

	var logBuf bytes.Buffer
	c := newSessionCache(pool.New(2), func(ctx context.Context) (observer.Handle, error) {
		return &fakeHandle{}, nil
	})
	c.Log = logging.NewAtLevel(&logBuf, "", 0, logging.Debug)

	a, err := c.getOrCreate(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	a.lastUsed = time.Now().Add(-30 * time.Second)
	b, err := c.getOrCreate(context.Background(), "newer")
	if err != nil {
		t.Fatal(err)
	}
	b.lastUsed = time.Now().Add(-5 * time.Second)

	start := time.Now()
	for i := 0; i < 6; i++ {
		if _, err := c.getOrCreate(context.Background(), fmt.Sprintf("arriving-%d", i)); err != nil {
			t.Fatalf("arriving session %d was refused a slot: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("six arrivals took %s; eviction must be immediate, not a wait", elapsed)
	}
	if c.size() != 2 {
		t.Fatalf("cache holds %d sessions, want exactly the capacity (2)", c.size())
	}
	if _, ok := c.byID["old"]; ok {
		t.Fatal("the least-recently-used session was not the one evicted first")
	}
	if got := c.poolInFlight(); got != 2 {
		t.Fatalf("pool in-flight = %d, want 2 — a slot leaked or was double-released", got)
	}
	if !strings.Contains(logBuf.String(), "evicted the least-recently-used") {
		t.Fatalf("eviction was not logged:\n%s", logBuf.String())
	}
}

// A session with a turn in flight is never evicted to make room.
func TestSessionSlotNeverEvictsAnInFlightTurn(t *testing.T) {
	orig := sessionSlotWait
	sessionSlotWait = 100 * time.Millisecond
	t.Cleanup(func() { sessionSlotWait = orig })
	c := newSessionCache(pool.New(1), func(ctx context.Context) (observer.Handle, error) {
		return &fakeHandle{}, nil
	})
	busy, err := c.getOrCreate(context.Background(), "busy")
	if err != nil {
		t.Fatal(err)
	}
	busy.mu.Lock()
	defer busy.mu.Unlock()
	if _, err := c.getOrCreate(context.Background(), "arriving"); err == nil {
		t.Fatal("an arriving session displaced a session mid-turn")
	}
	if _, ok := c.byID["busy"]; !ok {
		t.Fatal("the in-flight session was evicted")
	}
}

// TestSessionSlotWaiterEvictsOnceTheTurnFinishes pins the second half of
// the same end-to-end failure: arrivals that found every slot mid-turn
// must not sit out the whole deadline — as soon as a turn ends and that
// session is idle, the waiter evicts it and proceeds.
func TestSessionSlotWaiterEvictsOnceTheTurnFinishes(t *testing.T) {
	orig, origRetry := sessionSlotWait, slotRetryInterval
	sessionSlotWait, slotRetryInterval = 5*time.Second, 20*time.Millisecond
	t.Cleanup(func() { sessionSlotWait, slotRetryInterval = orig, origRetry })
	c := newSessionCache(pool.New(1), func(ctx context.Context) (observer.Handle, error) {
		return &fakeHandle{}, nil
	})
	busy, err := c.getOrCreate(context.Background(), "busy")
	if err != nil {
		t.Fatal(err)
	}
	busy.mu.Lock() // turn in flight
	go func() { time.Sleep(150 * time.Millisecond); busy.mu.Unlock() }()

	start := time.Now()
	if _, err := c.getOrCreate(context.Background(), "arriving"); err != nil {
		t.Fatalf("arriving session was dropped although the busy turn finished: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %s; should proceed right after the turn ended (~150ms)", elapsed)
	}
	if _, ok := c.byID["busy"]; ok {
		t.Fatal("the now-idle session should have been evicted for the arrival")
	}
}
