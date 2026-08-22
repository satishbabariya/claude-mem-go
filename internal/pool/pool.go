// Package pool bounds how many observer sessions run at once — the Go
// analog of src/supervisor/process-registry.ts's waitForSlot()/
// SlotReservation, called from ClaudeProvider.ts with maxConcurrent :=
// CLAUDE_MEM_MAX_CONCURRENT_AGENTS (default 2). Deliberately not part of
// claude-agent-sdk-go: a Session is one subprocess: how many run
// concurrently is this application's policy, not the SDK's concern.
package pool

import (
	"context"
	"time"
)

// Pool is a buffered-channel semaphore: Acquire blocks until a slot is
// free, Release frees it. This is admission control only — same contract
// as the real spawn-lock family: it gates SPAWNING, never anything else,
// and a held slot must never fail a caller, only make it wait.
type Pool struct {
	sem chan struct{}
}

// New creates a Pool allowing maxConcurrent concurrent holders.
//
// maxConcurrent below 1 is clamped to 1, not passed straight through — a
// real, reproducible bug found by hand, not anticipated: Go's own
// `make(chan T, n)` panics with "makechan: size out of range" for a
// negative n, so `pool.New(-1)` (e.g. a mistyped or computed
// `-max-concurrent` flag reaching the worker daemon at startup) crashed
// the process before it ever bound its socket. Zero has a quieter but
// equally real failure mode: a zero-capacity semaphore can never be
// acquired, so every single Acquire call would block forever — a
// permanent hang instead of a crash, not meaningfully better. Both clamp
// to the same safe floor since neither has a sane non-degenerate meaning
// for "how many observer sessions run at once."
func New(maxConcurrent int) *Pool {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Pool{sem: make(chan struct{}, maxConcurrent)}
}

// Acquire blocks until a slot is free, then reserves it.
func (p *Pool) Acquire() { p.sem <- struct{}{} }

// TryAcquire reserves a slot without blocking, reporting whether it got
// one. Lets a caller notice contention and say so before committing to a
// wait — the difference between a stall nobody can see and a logged one.
func (p *Pool) TryAcquire() bool {
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// AcquireWithin waits up to d for a slot, reporting whether it got one.
//
// The unbounded Acquire above is a genuine hazard for this project's
// actual usage, not a theoretical one. A slot is held for a cached
// observer session's whole LIFETIME, not per observation, and sessions
// stay cached for sessionIdleTimeout (10 minutes). With the default
// capacity of 2, a third concurrent Claude Code session therefore blocks
// here — with no timeout, no log line, and every health check still
// reporting green.
//
// Reproduced with three real concurrent sessions: two were captured, the
// third was accepted by the daemon and silently never processed, and
// `pool_in_flight: 2 / pool_capacity: 2` with 0% CPU was the only
// evidence anywhere that anything was wrong.
//
// A bounded wait converts that into something finite and reportable. The
// event is still lost when the deadline passes, but it was already lost
// in the unbounded case — the session ends long before a 10-minute
// eviction — and blocking also pinned a goroutine and its connection for
// the duration.
func (p *Pool) AcquireWithin(d time.Duration) bool {
	if p.TryAcquire() {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case p.sem <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

// AcquireContext is AcquireWithin with a ctx as well: it reports false
// when either d elapses or ctx is done, whichever comes first. The worker
// daemon's event path carries its shutdown ctx, and a caller waiting the
// full sessionSlotWait for a slot during shutdown was holding up the
// drain for a session that will never be spawned.
func (p *Pool) AcquireContext(ctx context.Context, d time.Duration) bool {
	if p.TryAcquire() {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case p.sem <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Release frees a slot acquired with Acquire. Callers should always
// `defer p.Release()` right after a successful Acquire.
func (p *Pool) Release() { <-p.sem }

// InFlight reports how many slots are currently held — the pool's only
// piece of runtime introspection, added so a caller (the worker daemon's
// stats snapshot) can report concurrency utilization instead of it being
// entirely opaque from outside. Safe to call concurrently with
// Acquire/Release: len() on a channel is a supported concurrent read, and
// this is a point-in-time snapshot, not a value anything should
// synchronize on.
func (p *Pool) InFlight() int { return len(p.sem) }

// Capacity is the maxConcurrent this Pool was created with.
func (p *Pool) Capacity() int { return cap(p.sem) }
