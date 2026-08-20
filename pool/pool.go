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
func New(maxConcurrent int) *Pool {
	return &Pool{sem: make(chan struct{}, maxConcurrent)}
}

// Acquire blocks until a slot is free, then reserves it.
func (p *Pool) Acquire() { p.sem <- struct{}{} }

// Release frees a slot acquired with Acquire. Callers should always
// `defer p.Release()` right after a successful Acquire.
func (p *Pool) Release() { <-p.sem }
