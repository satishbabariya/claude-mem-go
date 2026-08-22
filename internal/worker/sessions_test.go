package worker

import (
	"bytes"
	"context"
	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/pool"
	"github.com/satishbabariya/claude-mem-go/internal/store"
	"github.com/satishbabariya/claude-mem-go/internal/transcript"
)

// fakeHandle is a no-subprocess stand-in for *observer.Observer, so
// sessionCache's own logic (reuse, eviction, concurrency accounting) can be
// tested without spawning a real claude process.
type fakeHandle struct {
	id     int
	closed int32
}

func (f *fakeHandle) Observe(tc transcript.ToolCall) (observer.Turn, error) {
	return observer.Turn{}, nil
}
func (f *fakeHandle) Close() error   { atomic.StoreInt32(&f.closed, 1); return nil }
func (f *fakeHandle) isClosed() bool { return atomic.LoadInt32(&f.closed) == 1 }

func newFakeFactory() (func(ctx context.Context) (observer.Handle, error), *int32) {
	var counter int32
	factory := func(ctx context.Context) (observer.Handle, error) {
		n := atomic.AddInt32(&counter, 1)
		return &fakeHandle{id: int(n)}, nil
	}
	return factory, &counter
}

// newTrackingFakeFactory is like newFakeFactory but also records every
// handle it ever created, so a test can inspect ALL of them afterward —
// including ones that lost a getOrCreate race and were supposed to be
// closed and discarded.
func newTrackingFakeFactory() (factory func(ctx context.Context) (observer.Handle, error), all func() []*fakeHandle) {
	var mu sync.Mutex
	var created []*fakeHandle
	factory = func(ctx context.Context) (observer.Handle, error) {
		mu.Lock()
		defer mu.Unlock()
		h := &fakeHandle{id: len(created) + 1}
		created = append(created, h)
		return h, nil
	}
	all = func() []*fakeHandle {
		mu.Lock()
		defer mu.Unlock()
		out := make([]*fakeHandle, len(created))
		copy(out, created)
		return out
	}
	return factory, all
}

func TestSessionCacheReusesExistingSession(t *testing.T) {
	factory, spawnCount := newFakeFactory()
	c := newSessionCache(pool.New(2), factory)

	e1, err := c.getOrCreate(context.Background(), "session-a")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	e2, err := c.getOrCreate(context.Background(), "session-a")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	if e1 != e2 {
		t.Fatal("getOrCreate for the same session_id returned different entries — no reuse happened")
	}
	if got := atomic.LoadInt32(spawnCount); got != 1 {
		t.Fatalf("factory called %d times, want exactly 1 (second call should have reused the cache)", got)
	}
}

func TestSessionCacheDifferentSessionsGetDifferentHandles(t *testing.T) {
	factory, spawnCount := newFakeFactory()
	c := newSessionCache(pool.New(2), factory)

	ea, _ := c.getOrCreate(context.Background(), "session-a")
	eb, _ := c.getOrCreate(context.Background(), "session-b")
	if ea == eb {
		t.Fatal("different session_ids reused the same entry")
	}
	if got := atomic.LoadInt32(spawnCount); got != 2 {
		t.Fatalf("factory called %d times, want 2", got)
	}
	if c.size() != 2 {
		t.Fatalf("cache size = %d, want 2", c.size())
	}
}

func TestSessionCacheEvictClosesAndReleasesSlot(t *testing.T) {
	factory, _ := newFakeFactory()
	p := pool.New(1)
	c := newSessionCache(p, factory)

	e, err := c.getOrCreate(context.Background(), "session-a")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	fh := e.handle.(*fakeHandle)

	c.evict("session-a")

	if !fh.isClosed() {
		t.Fatal("evict did not close the handle")
	}
	if c.size() != 0 {
		t.Fatalf("cache size after evict = %d, want 0", c.size())
	}

	// The pool slot must actually be free again — Acquire should not block.
	done := make(chan struct{})
	go func() {
		p.Acquire()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("pool slot was not released by evict — Acquire is still blocked")
	}
}

func TestSessionCacheEvictNonexistentIsANoop(t *testing.T) {
	factory, _ := newFakeFactory()
	c := newSessionCache(pool.New(2), factory)
	c.evict("never-existed") // must not panic or misbehave
}

func TestSessionCacheCloseAllClosesEverySession(t *testing.T) {
	factory, _ := newFakeFactory()
	c := newSessionCache(pool.New(5), factory)

	var handles []*fakeHandle
	for _, id := range []string{"a", "b", "c"} {
		e, err := c.getOrCreate(context.Background(), id)
		if err != nil {
			t.Fatalf("getOrCreate: %v", err)
		}
		handles = append(handles, e.handle.(*fakeHandle))
	}

	c.closeAll()

	if c.size() != 0 {
		t.Fatalf("cache size after closeAll = %d, want 0", c.size())
	}
	for _, h := range handles {
		if !h.isClosed() {
			t.Fatalf("handle %d was not closed by closeAll", h.id)
		}
	}
}

// TestSessionCacheCloseAllWaitsForInFlightTurn is the regression test
// for the identical concurrency gap evictIdle had before its own fix,
// found on the shutdown path: closeAll used to call the raw evict (no
// entry.mu awareness at all) on every session regardless of whether a
// turn was genuinely still in flight — the same use-after/during-close
// hazard on the subprocess's pipes. Unlike evictIdle, closeAll can't
// just skip an in-flight session (there's no "next sweep" at shutdown),
// so the fix waits instead: simulates a turn that finishes comfortably
// within the (shrunk, for this test) grace period, and confirms
// closeAll doesn't close the handle until AFTER that turn's own
// entry.mu.Unlock() — proving it genuinely waited rather than closing
// out from under it.
func TestSessionCacheCloseAllWaitsForInFlightTurn(t *testing.T) {
	original := closeAllGracePeriod
	closeAllGracePeriod = 2 * time.Second
	t.Cleanup(func() { closeAllGracePeriod = original })

	release := make(chan struct{})
	handle := &blockingHandle{release: release}
	factory := func(ctx context.Context) (observer.Handle, error) { return handle, nil }
	c := newSessionCache(pool.New(1), factory)

	entry, err := c.getOrCreate(context.Background(), "in-flight")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}

	started := make(chan struct{})
	go func() {
		entry.mu.Lock()
		close(started)
		entry.handle.Observe(transcript.ToolCall{})
		entry.mu.Unlock()
	}()
	<-started

	closeAllDone := make(chan struct{})
	go func() {
		c.closeAll()
		close(closeAllDone)
	}()

	// closeAll must not have closed the handle yet — the simulated turn
	// is still genuinely in flight and well within its grace period.
	time.Sleep(50 * time.Millisecond)
	if handle.isClosed() {
		t.Fatal("closeAll closed the handle while a turn was still genuinely in flight, well within its grace period")
	}

	close(release) // let the simulated turn finish naturally

	select {
	case <-closeAllDone:
	case <-time.After(3 * time.Second):
		t.Fatal("closeAll did not return after the in-flight turn finished")
	}
	if !handle.isClosed() {
		t.Fatal("closeAll did not close the handle after the turn finished")
	}
}

// TestSessionCacheCloseAllForceClosesAfterGracePeriod confirms closeAll
// doesn't wait forever for a runaway turn: once the (shrunk, for this
// test) grace period elapses with the turn still in flight, it
// force-closes anyway rather than blocking shutdown indefinitely.
func TestSessionCacheCloseAllForceClosesAfterGracePeriod(t *testing.T) {
	original := closeAllGracePeriod
	closeAllGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { closeAllGracePeriod = original })

	release := make(chan struct{})
	defer close(release) // let the background goroutine's Observe return eventually, so it doesn't leak past the test
	handle := &blockingHandle{release: release}
	factory := func(ctx context.Context) (observer.Handle, error) { return handle, nil }
	c := newSessionCache(pool.New(1), factory)

	entry, err := c.getOrCreate(context.Background(), "runaway")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}

	started := make(chan struct{})
	go func() {
		entry.mu.Lock()
		close(started)
		entry.handle.Observe(transcript.ToolCall{}) // blocks until the test's own deferred close(release)
		entry.mu.Unlock()
	}()
	<-started

	closeAllDone := make(chan struct{})
	go func() {
		c.closeAll()
		close(closeAllDone)
	}()

	select {
	case <-closeAllDone:
	case <-time.After(2 * time.Second):
		t.Fatal("closeAll did not return within a bounded time of a runaway turn — it should force-close after its grace period rather than block shutdown forever")
	}
	if !handle.isClosed() {
		t.Fatal("closeAll did not force-close the handle after its grace period elapsed")
	}
}

// TestSessionCacheConcurrentGetOrCreateForSameIDDedupes covers the real risk:
// many PostToolUse events for the same session_id arriving close together.
// The cache uses double-checked locking, which deliberately allows more
// than one goroutine to race past the first check and spawn a handle before
// either registers — that's a legitimate tradeoff (don't serialize
// DIFFERENT sessions' spawns behind one lock) for occasionally wasting a
// spawn on a true race for the SAME session_id, which is rare in real usage
// (Claude Code's tool calls within one session are sequential). What must
// still hold: exactly one entry survives in the cache, every goroutine gets
// that same entry, and every handle that lost the race was actually closed
// — no leaked subprocess, no leaked pool slot.
func TestSessionCacheConcurrentGetOrCreateForSameIDDedupes(t *testing.T) {
	factory, allHandles := newTrackingFakeFactory()
	c := newSessionCache(pool.New(10), factory)

	const n = 20
	var wg sync.WaitGroup
	entries := make([]*sessionEntry, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := c.getOrCreate(context.Background(), "shared-session")
			if err != nil {
				t.Errorf("getOrCreate: %v", err)
				return
			}
			entries[i] = e
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if entries[i] != entries[0] {
			t.Fatalf("goroutine %d got a different entry than goroutine 0 — every caller must share one entry", i)
		}
	}
	if c.size() != 1 {
		t.Fatalf("cache size = %d, want exactly 1 surviving entry", c.size())
	}

	winner := entries[0].handle.(*fakeHandle)
	var openCount, closedCount int
	for _, h := range allHandles() {
		if h.isClosed() {
			closedCount++
		} else {
			openCount++
			if h != winner {
				t.Errorf("handle %d is open but is not the entry the cache registered — leaked subprocess", h.id)
			}
		}
	}
	if openCount != 1 {
		t.Fatalf("%d handles left open, want exactly 1 (the winner); %d were correctly closed", openCount, closedCount)
	}
}

func TestSessionCacheEvictIdleOnlyRemovesStaleSessions(t *testing.T) {
	factory, _ := newFakeFactory()
	c := newSessionCache(pool.New(2), factory)

	fresh, _ := c.getOrCreate(context.Background(), "fresh")
	stale, _ := c.getOrCreate(context.Background(), "stale")

	// Back-date only the "stale" entry.
	c.mu.Lock()
	c.byID["stale"].lastUsed = time.Now().Add(-2 * sessionIdleTimeout)
	c.mu.Unlock()

	c.evictIdle()

	if c.size() != 1 {
		t.Fatalf("cache size after evictIdle = %d, want 1 (only the stale one removed)", c.size())
	}
	if !stale.handle.(*fakeHandle).isClosed() {
		t.Fatal("stale session was not closed by evictIdle")
	}
	if fresh.handle.(*fakeHandle).isClosed() {
		t.Fatal("fresh session was incorrectly closed by evictIdle")
	}
}

// TestSessionCacheTouchRefreshesLastUsed is the regression test for the
// other half of the real gap evictIdle's fix depends on: lastUsed was
// only ever refreshed when a turn STARTED (getOrCreate), never when one
// finished — so a session's idle clock was measured from turn start, not
// real last-activity time. touch (called by worker.Daemon.process after
// a turn finishes) closes that gap. Confirms it actually updates the
// cached entry, not a copy, and is a safe no-op for an unknown session
// (already evicted by the time a caller gets around to touching it,
// which is a real possible ordering, not just defensive padding).
func TestSessionCacheTouchRefreshesLastUsed(t *testing.T) {
	factory, _ := newFakeFactory()
	c := newSessionCache(pool.New(2), factory)

	entry, err := c.getOrCreate(context.Background(), "s1")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	c.mu.Lock()
	entry.lastUsed = old
	c.mu.Unlock()

	c.touch("s1")

	c.mu.Lock()
	got := c.byID["s1"].lastUsed
	c.mu.Unlock()
	if !got.After(old) {
		t.Fatalf("lastUsed after touch = %v, want a time after the back-dated %v", got, old)
	}

	c.touch("never-existed") // must not panic
}

// slowFakeHandle is an observer.Handle whose Observe takes a fixed,
// artificial delay before returning — lets a test distinguish "lastUsed
// was refreshed at the START of a turn" (getOrCreate's own, pre-existing
// behavior) from "lastUsed was refreshed when the turn FINISHED" (touch,
// this fix): only the latter could push lastUsed past the delay.
type slowFakeHandle struct {
	delay time.Duration
}

func (h *slowFakeHandle) Observe(tc transcript.ToolCall) (observer.Turn, error) {
	time.Sleep(h.delay)
	return observer.Turn{}, nil
}
func (h *slowFakeHandle) Close() error { return nil }

// TestProcessTouchesSessionAfterSuccessfulTurn confirms Daemon.process
// itself refreshes lastUsed when a turn FINISHES, not just relying on
// getOrCreate's own pre-existing start-of-turn refresh — a real
// regression would be forgetting to wire the touch call at process's one
// real call site, or reverting to only the start-of-turn refresh, which
// a unit test of touch alone (or a naive "did lastUsed advance at all"
// check here) can't distinguish, since getOrCreate refreshes it before
// Observe is ever called regardless. Uses a slowFakeHandle with an
// artificial delay: only an end-of-turn refresh could push lastUsed past
// that delay from the call's own start time.
func TestProcessTouchesSessionAfterSuccessfulTurn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	const delay = 50 * time.Millisecond
	entry := &sessionEntry{handle: &slowFakeHandle{delay: delay}}

	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0), st: st}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{"s1": entry}}

	beforeCall := time.Now()
	payload := []byte(`{"session_id":"s1","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)
	d.process(context.Background(), payload)

	d.sessions.mu.Lock()
	got := d.sessions.byID["s1"].lastUsed
	d.sessions.mu.Unlock()
	if got.Before(beforeCall.Add(delay)) {
		t.Fatalf("lastUsed = %v, want at least %v after the call started (the observer's own %s artificial delay) — a start-of-turn-only refresh could not have advanced lastUsed this far, meaning the end-of-turn touch call doesn't appear to have fired", got, beforeCall.Add(delay), delay)
	}
}

// TestSessionCacheEvictIdleSkipsSessionWithTurnInFlight is the direct
// regression test for a real bug found by hand, not hypothetical: a
// session's lastUsed is refreshed when a turn starts (and, since this
// fix, when one finishes — see touch), but nothing bounds how long a
// single turn itself can run. This project has measured a real single
// observation taking 104 seconds under normal load — comfortably enough
// to look "idle" by a stale lastUsed timestamp if the timeout window is
// tight, and there's no guarantee a turn can never legitimately run even
// longer. The original evictIdle evicted purely on that timestamp, with
// no awareness of entry.mu — meaning a turn genuinely still in flight
// could have its handle closed out from under it, a real
// use-after/during-close hazard on the underlying subprocess's pipes,
// not just a wasted turn.
//
// Simulates worker.Daemon.process's own critical section directly
// (acquiring entry.mu and calling Observe, exactly as process does) with
// a handle whose Observe blocks on command, back-dates lastUsed to look
// idle while that simulated turn is still genuinely in flight, and
// confirms evictIdle leaves it alone. Then releases the simulated turn,
// re-stales lastUsed, and confirms a LATER sweep does evict it — proving
// the fix defers eviction rather than leaking the session forever.
func TestSessionCacheEvictIdleSkipsSessionWithTurnInFlight(t *testing.T) {
	release := make(chan struct{})
	handle := &blockingHandle{release: release}
	factory := func(ctx context.Context) (observer.Handle, error) { return handle, nil }
	c := newSessionCache(pool.New(1), factory)

	entry, err := c.getOrCreate(context.Background(), "long-turn")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	c.mu.Lock()
	entry.lastUsed = time.Now().Add(-2 * sessionIdleTimeout)
	c.mu.Unlock()

	started := make(chan struct{})
	turnDone := make(chan struct{})
	go func() {
		entry.mu.Lock()
		close(started)
		entry.handle.Observe(transcript.ToolCall{})
		entry.mu.Unlock()
		close(turnDone)
	}()
	<-started // entry.mu is now held, simulating a genuinely in-flight turn

	c.evictIdle()

	if handle.isClosed() {
		t.Fatal("evictIdle closed a session with a turn genuinely still in flight — the real use-after/during-close hazard this fix exists to prevent")
	}
	if c.size() != 1 {
		t.Fatalf("cache size after evictIdle = %d, want 1 (the in-flight session must not be removed from the cache either)", c.size())
	}

	close(release) // let the simulated turn finish
	select {
	case <-turnDone:
	case <-time.After(2 * time.Second):
		t.Fatal("simulated turn did not finish")
	}

	// Now that the turn has genuinely finished, a later sweep must still
	// be able to evict it — the fix defers eviction, it doesn't disable
	// it.
	c.mu.Lock()
	entry.lastUsed = time.Now().Add(-2 * sessionIdleTimeout)
	c.mu.Unlock()
	c.evictIdle()
	if !handle.isClosed() {
		t.Fatal("evictIdle did not close the session on a later sweep once its turn had genuinely finished")
	}
	if c.size() != 0 {
		t.Fatalf("cache size after the second evictIdle = %d, want 0", c.size())
	}
}
