// sessionCache reuses one observer.Handle per Claude Code session_id across
// PostToolUse events, instead of spawning a fresh `claude` subprocess for
// every single tool call.
//
// This matters for two reasons, both visible in this project's own real
// test runs: cost (repeated turns on the same session showed
// cache_read_input_tokens rising and per-turn cost dropping as context
// accumulated — a fresh subprocess per call throws that away every time)
// and latency (every spawn pays a cold-start cost). It mirrors
// claude-mem's real ClaudeProvider.ts, which keeps one query() session per
// content session for exactly this reason, rather than one per tool call.
package worker

import (
	"context"
	"sync"
	"time"

	"claude-mem-go/observer"
	"claude-mem-go/pool"
)

// sessionIdleTimeout bounds how long an unused cached session is kept
// alive. claude-mem's real system tears a session down on its Stop event
// instead of an idle timer; this daemon has no equivalent signal (a
// PostToolUse-only protocol doesn't tell it when a session ended), so an
// idle timer is the pragmatic substitute — bounds a long-lived daemon's
// resource use without needing a "session ended" event this protocol
// doesn't carry.
const sessionIdleTimeout = 10 * time.Minute

type sessionEntry struct {
	// mu serializes turns on this one subprocess — Observe must never be
	// called concurrently on the same underlying pipe, since two turns
	// racing on the same stdin/stdout would interleave into nonsense.
	mu       sync.Mutex
	handle   observer.Handle
	lastUsed time.Time
}

// sessionCache maps a session_id to its one persistent observer.Handle.
// newFunc is a factory rather than a hardcoded observer.New call so tests
// can substitute a fake handle instead of spawning a real claude
// subprocess per test case.
type sessionCache struct {
	mu      sync.Mutex
	byID    map[string]*sessionEntry
	pool    *pool.Pool
	newFunc func(ctx context.Context) (observer.Handle, error)
}

func newSessionCache(p *pool.Pool, newFunc func(ctx context.Context) (observer.Handle, error)) *sessionCache {
	return &sessionCache{byID: make(map[string]*sessionEntry), pool: p, newFunc: newFunc}
}

// getOrCreate returns sessionID's cached entry, creating one (behind a pool
// slot) if none exists yet. The pool is acquired OUTSIDE the cache's own
// lock — spawning a subprocess can take a moment, and holding the map lock
// during that would block unrelated sessions from even checking the cache.
func (c *sessionCache) getOrCreate(ctx context.Context, sessionID string) (*sessionEntry, error) {
	c.mu.Lock()
	if e, ok := c.byID[sessionID]; ok {
		e.lastUsed = time.Now()
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()

	c.pool.Acquire()
	h, err := c.newFunc(ctx)
	if err != nil {
		c.pool.Release()
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byID[sessionID]; ok {
		// Lost a race with a concurrent getOrCreate for the same
		// session_id — keep the entry already registered, discard ours.
		h.Close()
		c.pool.Release()
		e.lastUsed = time.Now()
		return e, nil
	}
	e := &sessionEntry{handle: h, lastUsed: time.Now()}
	c.byID[sessionID] = e
	return e, nil
}

// evict closes and removes sessionID's entry, if present. Used both for
// idle cleanup and — importantly — whenever a turn on a cached session
// fails: a broken subprocess must not stay cached to fail identically on
// every future turn for that session, and would otherwise leak its pool
// slot forever.
func (c *sessionCache) evict(sessionID string) {
	c.mu.Lock()
	e, ok := c.byID[sessionID]
	if ok {
		delete(c.byID, sessionID)
	}
	c.mu.Unlock()
	if ok {
		e.handle.Close()
		c.pool.Release()
	}
}

// evictIdle closes every session idle longer than sessionIdleTimeout.
// Meant to run periodically in the background.
func (c *sessionCache) evictIdle() {
	c.mu.Lock()
	var stale []string
	for id, e := range c.byID {
		if time.Since(e.lastUsed) > sessionIdleTimeout {
			stale = append(stale, id)
		}
	}
	c.mu.Unlock()
	for _, id := range stale {
		c.evict(id)
	}
}

// closeAll evicts every session — used on daemon shutdown.
func (c *sessionCache) closeAll() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.byID))
	for id := range c.byID {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.evict(id)
	}
}

func (c *sessionCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byID)
}

// poolInFlight and poolCapacity expose the underlying pool's utilization —
// part of the worker's observability surface (see Daemon.Stats), not used
// for any control-flow decision here.
func (c *sessionCache) poolInFlight() int { return c.pool.InFlight() }
func (c *sessionCache) poolCapacity() int { return c.pool.Capacity() }
