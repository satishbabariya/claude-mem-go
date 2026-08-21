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

// privacyFlag is the current session's "was the prompt that started this
// turn entirely private" state — see setPrivate/isPrivate. setAt exists
// purely so evictStalePrivacy can bound this map's lifetime the same way
// sessionIdleTimeout already bounds byID's; it has nothing to do with the
// privacy decision itself.
type privacyFlag struct {
	private bool
	setAt   time.Time
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

	// privMu/priv are deliberately separate from mu/byID: setPrivate is
	// called by the UserPromptSubmit hook (prompt-context) for EVERY
	// prompt, private or not, often before this session has any
	// sessionEntry at all (a purely-conversational turn with no tool
	// calls never creates one) — it must never block on, or trigger,
	// getOrCreate's subprocess-spawning path just to record a flag.
	privMu sync.Mutex
	priv   map[string]privacyFlag
}

func newSessionCache(p *pool.Pool, newFunc func(ctx context.Context) (observer.Handle, error)) *sessionCache {
	return &sessionCache{byID: make(map[string]*sessionEntry), pool: p, newFunc: newFunc}
}

// setPrivate records whether sessionID's most recently submitted prompt was
// entirely private (see privacy.StripMemoryTags) — called on every single
// UserPromptSubmit, with both true and false, not just when private:
// real claude-mem's own equivalent (PrivacyCheckValidator) re-checks a
// specific prompt_number's persisted row fresh each time rather than
// relying on a sticky flag, but this port has no equivalent per-turn
// table to look up — a flag that only ever gets set to true and never
// reset would silently suppress capture for every later turn in the
// session too, which is a worse bug than the one this exists to fix. The
// UserPromptSubmit hook is NOT registered async in hooks.json — it blocks
// Claude Code's turn until it returns — so by wall-clock ordering this
// call always lands before any PostToolUse event for a tool call made in
// response to that same prompt can possibly arrive.
func (c *sessionCache) setPrivate(sessionID string, private bool) {
	c.privMu.Lock()
	defer c.privMu.Unlock()
	if c.priv == nil {
		c.priv = make(map[string]privacyFlag)
	}
	c.priv[sessionID] = privacyFlag{private: private, setAt: time.Now()}
}

// isPrivate reports sessionID's current privacy flag — false (not
// private) both when it was explicitly cleared AND when it was never set
// at all, matching real claude-mem's own documented default: an absent
// signal must never be treated as "private," since that would silently
// freeze every observation for a session whose UserPromptSubmit simply
// hasn't reported in yet (the exact #2794/#2795 bug its own
// PrivacyCheckValidator doc comment describes and deliberately avoids).
func (c *sessionCache) isPrivate(sessionID string) bool {
	c.privMu.Lock()
	defer c.privMu.Unlock()
	return c.priv[sessionID].private
}

// evictStalePrivacy bounds priv's lifetime the same way evictIdle bounds
// byID's, so a daemon that has ever seen a session doesn't grow this map
// forever. Meant to run periodically alongside evictIdle.
func (c *sessionCache) evictStalePrivacy() {
	c.privMu.Lock()
	defer c.privMu.Unlock()
	for id, f := range c.priv {
		if time.Since(f.setAt) > sessionIdleTimeout {
			delete(c.priv, id)
		}
	}
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

// touch refreshes sessionID's lastUsed to now, if it's still cached.
// getOrCreate already refreshes it when a turn STARTS; this covers the
// other end — called when a turn actually FINISHES (worker.go's
// process, after a successful Observe). Without this, a session's idle
// clock was measured from when its last turn started, not when it
// actually finished being used: for a real long-running turn (this
// project has measured a single real observation taking over a minute),
// the session could look far closer to sessionIdleTimeout than it truly
// was the instant that turn completed, and evictIdle's own safety margin
// below depends on lastUsed meaning "last real activity," not "last
// turn's start time."
func (c *sessionCache) touch(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byID[sessionID]; ok {
		e.lastUsed = time.Now()
	}
}

// evictIdle closes every session idle longer than sessionIdleTimeout —
// but only when no turn is currently in flight for it. Meant to run
// periodically in the background.
//
// A real bug found by hand, not hypothetical: lastUsed is refreshed when
// a turn starts (and now, via touch, when one finishes), but nothing
// bounds how long a single turn itself can run — this project has
// measured a real single observation taking 104 seconds under normal
// load, comfortably longer than plenty of margin inside
// sessionIdleTimeout's own window on a busy daemon, and there's no
// guarantee one can never legitimately run even longer. The original
// version of this function evicted purely on a stale lastUsed timestamp,
// with no awareness of entry.mu — meaning a turn genuinely still in
// flight past the idle window could have its handle closed out from
// under it: closing the underlying subprocess's stdin while
// worker.Daemon.process is still blocked reading its stdout inside
// Observe is a real use-after/during-close hazard, not just a wasted
// turn (traced one level deeper into claude-agent-sdk-go's own
// Session.Close/Send: no synchronization between them either, so this
// really would race two goroutines over one subprocess's pipes).
//
// entry.mu.TryLock (not a blocking Lock) is what makes this safe:
// succeeding PROVES no turn is currently running — process() isn't
// holding the lock — so it's genuinely safe to close. Failing means a
// turn IS active right now, and this sweep simply skips that session;
// it'll be reconsidered on the next sweep a minute later, which is
// always safe regardless of how long the turn takes, since a session
// with a turn actively in flight isn't meaningfully idle no matter what
// its stale lastUsed claims. The map re-check after acquiring the lock
// (cur == e) guards the (safe, already-handled-by-existing-retry-logic)
// case where a concurrent getOrCreate grabbed this same entry between
// the snapshot above and the lock being acquired here.
func (c *sessionCache) evictIdle() {
	c.mu.Lock()
	var stale []*sessionEntry
	staleID := make(map[*sessionEntry]string, len(c.byID))
	for id, e := range c.byID {
		if time.Since(e.lastUsed) > sessionIdleTimeout {
			stale = append(stale, e)
			staleID[e] = id
		}
	}
	c.mu.Unlock()

	for _, e := range stale {
		if !e.mu.TryLock() {
			continue // a turn is actively in flight — leave it for the next sweep
		}
		id := staleID[e]
		c.mu.Lock()
		cur, ok := c.byID[id]
		stillLive := ok && cur == e
		if stillLive {
			delete(c.byID, id)
		}
		c.mu.Unlock()
		e.mu.Unlock()
		if stillLive {
			e.handle.Close()
			c.pool.Release()
		}
	}
}

// closeAllGracePeriod bounds how long closeAll waits for an in-flight
// turn to finish naturally before force-closing its handle anyway. A
// var, not a const, so a test can shrink it.
//
// Unlike evictIdle's periodic sweep, shutdown has no "later" to defer
// to: the daemon is exiting either way, so simply skipping a session
// with a turn in flight (the way evictIdle safely can) would leak its
// subprocess as an orphan with nothing left to ever clean it up.
// Waiting — acquiring entry.mu the normal way, not a TryLock — is the
// correct default here; force-closing after the grace period is the
// one accepted exception to "never close while a turn might be in
// flight," reserved for a runaway turn that would otherwise block
// shutdown forever. Matches the same shape as this daemon's own metrics
// HTTP server shutdown (Run's metricsSrv.Shutdown with a 5s
// context.WithTimeout) — bounded grace period, then move on regardless.
var closeAllGracePeriod = 5 * time.Second

// closeAll closes every cached session — used on daemon shutdown. Found
// by hand as the identical concurrency gap evictIdle had before its own
// fix, on the shutdown path instead of the idle-timer path: the
// original version called evict (no entry.mu awareness at all) on every
// session regardless of whether a turn was still genuinely in flight —
// the same use-after/during-close hazard on the subprocess's pipes.
//
// This does NOT fully solve every shutdown race by itself: a
// handleConn/process goroutine already dispatched from Run's Accept
// loop before shutdown began could still be racing getOrCreate for a
// BRAND NEW session concurrently with this snapshot — a real, separate,
// broader "drain in-flight requests before closing anything" concern
// this fix doesn't attempt, scoped out deliberately rather than
// overreaching beyond the specific mutex-safety gap found.
func (c *sessionCache) closeAll() {
	c.mu.Lock()
	entries := make([]*sessionEntry, 0, len(c.byID))
	for _, e := range c.byID {
		entries = append(entries, e)
	}
	c.byID = make(map[string]*sessionEntry)
	c.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *sessionEntry) {
			defer wg.Done()
			acquired := make(chan struct{})
			go func() {
				e.mu.Lock()
				close(acquired)
			}()
			select {
			case <-acquired:
			case <-time.After(closeAllGracePeriod):
				// Grace period elapsed with the turn still running —
				// force-close anyway rather than block shutdown forever.
				// The lock-acquiring goroutine above will still
				// eventually succeed once that turn's own process() call
				// finishes and unlocks it, harmlessly discarded by then.
			}
			e.handle.Close()
			c.pool.Release()
		}(e)
	}
	wg.Wait()
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
