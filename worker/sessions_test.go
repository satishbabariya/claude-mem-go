package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-mem-go/observer"
	"claude-mem-go/pool"
	"claude-mem-go/transcript"
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
