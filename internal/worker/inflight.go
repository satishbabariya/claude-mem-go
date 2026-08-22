package worker

import "sync"

// inflightTracker counts, per session_id, how many PostToolUse events the
// daemon currently has between "received" and "fully processed" (through
// the observer LLM call, the store insert, and the embedding call) — not
// just events actively holding a sessionEntry.mu lock, since a second
// event for the same session queued behind the first is just as much
// "more observations are still coming" for a caller deciding whether it's
// safe to summarize a session as one whose lock is currently held.
//
// This is the direct fix for the architectural gap the Stop hook's own
// waitForSessionObservations doc comment named as unresolved: rather than
// inferring "has the worker caught up" from watching the observations
// table's row count stabilize (a heuristic that can be fooled by a
// sequential-processing plateau, and has no way to distinguish "genuinely
// done" from "still working" without guessing at a timeout), a caller can
// now ask the worker directly.
type inflightTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func newInflightTracker() *inflightTracker {
	return &inflightTracker{counts: make(map[string]int)}
}

func (t *inflightTracker) inc(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[sessionID]++
}

// dec removes the map entry entirely once a session's count reaches zero,
// rather than leaving a lingering zero-valued entry — this map is keyed by
// every session_id the daemon has ever seen, and never explicitly cleaned
// up otherwise (unlike sessionCache, which evicts on an idle timer).
func (t *inflightTracker) dec(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.counts[sessionID] - 1
	if n <= 0 {
		delete(t.counts, sessionID)
		return
	}
	t.counts[sessionID] = n
}

func (t *inflightTracker) count(sessionID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[sessionID]
}

// getInflight lazily initializes d.inflight, so a Daemon built directly in
// a test (rather than through Run) can still call process/handleConn
// without a nil-map panic.
func (d *Daemon) getInflight() *inflightTracker {
	d.inflightOnce.Do(func() {
		d.inflight = newInflightTracker()
	})
	return d.inflight
}
