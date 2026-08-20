// Package pool bounds how many observer sessions run at once — the Go
// analog of src/supervisor/process-registry.ts's waitForSlot()/
// SlotReservation, called from ClaudeProvider.ts with maxConcurrent :=
// CLAUDE_MEM_MAX_CONCURRENT_AGENTS (default 2). Deliberately not part of
// claude-agent-sdk-go: a Session is one subprocess: how many run
// concurrently is this application's policy, not the SDK's concern.
package pool

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
