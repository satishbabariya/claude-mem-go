package main

import (
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// setFastPollIntervalForTest shrinks stopWaitPollInterval for the
// duration of one test, restoring the real value afterward — lets these
// tests exercise the same iteration-count logic near-instantly instead of
// actually sleeping for real seconds per test case.
func setFastPollIntervalForTest(t *testing.T) {
	t.Helper()
	original := stopWaitPollInterval
	stopWaitPollInterval = time.Millisecond
	t.Cleanup(func() { stopWaitPollInterval = original })
}

// sequencedBackend is a minimal memory.Backend fake whose BySessionID
// returns one entry from counts per call (clamped to the last entry once
// exhausted), each call returning that many placeholder SearchResults —
// simulating the worker's own async pipeline persisting observations for
// a session one at a time, across several real seconds. Every other
// method is unused by waitForSessionObservations and left as an
// intentionally unimplemented panic, so a test that accidentally
// exercises one fails loudly rather than silently returning a zero value.
type sequencedBackend struct {
	memory.Backend
	counts []int
	calls  int
}

func (s *sequencedBackend) BySessionID(sessionID string, limit int) ([]memory.SearchResult, error) {
	i := s.calls
	if i >= len(s.counts) {
		i = len(s.counts) - 1
	}
	s.calls++
	n := s.counts[i]
	out := make([]memory.SearchResult, n)
	for j := range out {
		out[j] = memory.SearchResult{ID: int64(j + 1)}
	}
	return out, nil
}

// TestWaitForSessionObservationsStopsOnceCountStabilizes is the
// regression test for a real bug found by hand: Stop fires the instant a
// session ends, but PostToolUse's own observation for the session's LAST
// tool call is a real async LLM call the worker runs in the background —
// a single, immediate BySessionID call can race ahead of it. Simulates
// the worker's count growing 0 -> 1 -> 2 across the first few polls, then
// stabilizing at 2, and confirms waitForSessionObservations waits for
// that stabilization rather than accepting the first (incomplete) read.
func TestWaitForSessionObservationsStopsOnceCountStabilizes(t *testing.T) {
	setFastPollIntervalForTest(t)

	be := &sequencedBackend{counts: []int{0, 1, 2, 2, 2}}
	start := time.Now()
	got, err := waitForSessionObservations(be, "s1", 50, nil)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 2 (the stabilized count, not the first read of 0)", len(got))
	}
	// 4 polls needed to see 0,1,2,2 (the 4th confirms the 3rd) — at
	// stopWaitPollInterval each, this must have actually waited, not
	// returned instantly on the first (wrong) read.
	if elapsed := time.Since(start); elapsed < 3*stopWaitPollInterval {
		t.Errorf("waitForSessionObservations returned after %s, want it to have actually polled through the growing sequence", elapsed)
	}
}

// TestWaitForSessionObservationsGivesUpAfterMaxAttempts confirms a
// perpetually-growing count (one that never actually stabilizes) doesn't
// hang the hook forever — Stop's own timeout in hooks/hooks.json is
// generous but finite.
func TestWaitForSessionObservationsGivesUpAfterMaxAttempts(t *testing.T) {
	setFastPollIntervalForTest(t)

	counts := make([]int, stopWaitMaxAttempts+5)
	for i := range counts {
		counts[i] = i // never stabilizes
	}
	be := &sequencedBackend{counts: counts}
	got, err := waitForSessionObservations(be, "s1", 50, nil)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != stopWaitMaxAttempts-1 {
		t.Fatalf("waitForSessionObservations returned %d observations, want %d (whatever the last attempt saw before giving up)", len(got), stopWaitMaxAttempts-1)
	}
}

// TestWaitForSessionObservationsWaitsFullBudgetWhenGenuinelyEmpty is the
// regression test for a real flaw found in this fix's own FIRST version,
// caught live (not just in a unit test) before it shipped: treating two
// consecutive zero-reads as "stabilized, genuinely nothing to summarize"
// is wrong — a session with exactly one real tool call also reads 0 on
// every check until the worker's observer call actually finishes, which
// routinely took longer than a single poll interval in practice. The
// first version of this fix exited after the second zero-read and
// missed a real observation that landed on the third check. Only a count
// that has gone positive and then stops growing is real evidence of
// stability; a session that never produces any observation at all must
// pay the full wait budget, which is an acceptable cost since Stop runs
// fire-and-forget.
func TestWaitForSessionObservationsWaitsFullBudgetWhenGenuinelyEmpty(t *testing.T) {
	setFastPollIntervalForTest(t)

	be := &sequencedBackend{counts: []int{0, 0, 0, 0, 0}}
	got, err := waitForSessionObservations(be, "s1", 50, nil)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 0", len(got))
	}
	if be.calls != stopWaitMaxAttempts {
		t.Fatalf("waitForSessionObservations made %d calls, want exactly %d (the full wait budget) — a genuinely-empty session must not stabilize early on a zero count", be.calls, stopWaitMaxAttempts)
	}
}

// TestWaitForSessionObservationsDoesNotStabilizeFalselyAtZero reproduces
// the exact shape of the live bug above directly: the count reads zero
// on the first TWO checks (which a naive "two consecutive equal reads"
// rule would have already accepted as "stable at zero") before the
// worker's real observation shows up on the third check and then holds
// steady. Confirms the fix waits past that false stabilization instead
// of returning 0 early.
func TestWaitForSessionObservationsDoesNotStabilizeFalselyAtZero(t *testing.T) {
	setFastPollIntervalForTest(t)

	be := &sequencedBackend{counts: []int{0, 0, 1, 1, 1}}
	got, err := waitForSessionObservations(be, "s1", 50, nil)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 1 — it must not have stopped at the two leading zero-reads", len(got))
	}
}

// TestWaitForSessionObservationsWaitsPastAPlateauForASecondToolCall is the
// regression test for a second, more consequential real bug found live
// against an actual session with TWO tool calls, not just a unit test:
// this function's first version required only two consecutive matching
// reads to trust stability. A session with multiple tool calls processes
// them SEQUENTIALLY (see sessions.go's per-session mutex) — the first
// tool call's observation lands, the count holds at 1 for several real
// seconds while the SECOND tool call's observation is still mid-flight,
// and the old rule locked in at count=1 during that plateau, producing a
// summary that silently covered only the first of the two tool calls.
// Simulates exactly that shape: 0, then 1 held for 5 checks (fewer than
// stopStableStreakRequired, so the old 2-check rule would have already
// locked in here but the fix must not), then 2 held long enough to
// actually satisfy the real streak requirement.
func TestWaitForSessionObservationsWaitsPastAPlateauForASecondToolCall(t *testing.T) {
	setFastPollIntervalForTest(t)

	counts := []int{0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	be := &sequencedBackend{counts: counts}
	got, err := waitForSessionObservations(be, "s1", 50, nil)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 2 — it must not have locked in at the 5-check plateau of 1 (a second tool call's observation was still coming)", len(got))
	}
}

// TestWaitForSessionObservationsFallsBackWhenWorkerUnreachable confirms
// that an inFlight query which never succeeds (worker not running, wrong
// socket path, crashed — QueryInFlight's real failure modes) degrades
// cleanly to the exact same row-count heuristic as passing a nil inFlight
// func, rather than failing outright or misinterpreting "unknown" as
// "confirmed zero in flight." Same scenario and expected result as
// TestWaitForSessionObservationsStopsOnceCountStabilizes, just with an
// always-failing query in the mix.
func TestWaitForSessionObservationsFallsBackWhenWorkerUnreachable(t *testing.T) {
	setFastPollIntervalForTest(t)

	be := &sequencedBackend{counts: []int{0, 1, 2, 2, 2}}
	unreachable := func(sessionID string) (int, bool) { return 0, false }
	got, err := waitForSessionObservations(be, "s1", 50, unreachable)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 2 — an unreachable worker query must fall back to the row-count heuristic, not fail or misbehave", len(got))
	}
}

// TestWaitForSessionObservationsInFlightFastPathShortensWait is the
// regression test for the actual point of the in-flight query: once the
// worker daemon reports zero events in flight for this session for
// stopInFlightConfirmStreak consecutive checks, this must return almost
// immediately — NOT wait out the full stopStableStreakRequired=10
// row-count streak, which is what a nil (or always-failing) inFlight func
// would be stuck doing. Simulates a worker that's still processing for
// the first two queries, then confirms zero — traced by hand to return
// after exactly 4 BySessionID calls, far short of the ~11 the row-count
// heuristic alone would need for the same (deliberately unstabilized,
// constant-at-1) observation sequence.
func TestWaitForSessionObservationsInFlightFastPathShortensWait(t *testing.T) {
	setFastPollIntervalForTest(t)

	counts := make([]int, 30)
	for i := 1; i < len(counts); i++ {
		counts[i] = 1
	}
	be := &sequencedBackend{counts: counts}

	queries := 0
	inFlight := func(sessionID string) (int, bool) {
		queries++
		if queries <= 2 {
			return 1, true // worker still working
		}
		return 0, true // worker confirms nothing left in flight
	}

	got, err := waitForSessionObservations(be, "s1", 50, inFlight)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 1", len(got))
	}
	if be.calls != 4 {
		t.Fatalf("waitForSessionObservations made %d BySessionID calls, want exactly 4 — it should have exited via the in-flight fast path almost immediately, not waited toward stopStableStreakRequired=%d", be.calls, stopStableStreakRequired)
	}
}

// TestWaitForSessionObservationsExtendsBudgetPastOldCeilingWhenWorkerConfirmsActivity
// is the regression test for the real, live-observed case this whole
// redesign exists for: a single observation that took 104 seconds to
// process — longer than the original stopWaitMaxAttempts (~45s) ceiling
// — which would have been cut off mid-flight without a live signal
// justifying a longer wait. Simulates a first observation already
// persisted (count=1) while the worker reports a SECOND event genuinely
// in flight for far longer than stopWaitMaxAttempts attempts, then that
// second observation lands (count=2) right as the worker confirms
// nothing left in flight. Also exercises the override this test would
// have caught without: if the row-count streak were allowed to run
// unchecked while the worker says "busy," it would have locked in at
// count=1 around attempt ~11 (stopStableStreakRequired consecutive
// matches), long before the worker ever confirmed anything — this test
// only passes if that's actually prevented.
func TestWaitForSessionObservationsExtendsBudgetPastOldCeilingWhenWorkerConfirmsActivity(t *testing.T) {
	setFastPollIntervalForTest(t)

	const busyUntilQuery = 54 // well past stopWaitMaxAttempts=45
	counts := make([]int, busyUntilQuery+10)
	for i := range counts {
		switch {
		case i == 0:
			counts[i] = 0
		case i < busyUntilQuery:
			counts[i] = 1
		default:
			counts[i] = 2
		}
	}
	be := &sequencedBackend{counts: counts}

	queries := 0
	inFlight := func(sessionID string) (int, bool) {
		queries++
		if queries <= busyUntilQuery {
			return 1, true // second event still genuinely in flight
		}
		return 0, true
	}

	got, err := waitForSessionObservations(be, "s1", 50, inFlight)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 2 — it must wait for the worker-confirmed second event, not lock in at 1", len(got))
	}
	if be.calls <= stopWaitMaxAttempts {
		t.Fatalf("waitForSessionObservations made %d BySessionID calls, want more than stopWaitMaxAttempts=%d — the worker confirming real activity should have extended the wait ceiling", be.calls, stopWaitMaxAttempts)
	}
}

// TestWaitForSessionObservationsGenuinelyEmptySessionStillBoundedEvenWhenWorkerReachable
// is the non-regression counterpart to the extension test above: a
// session that never shows ANY activity at all — the worker is reachable
// and answers every query, but always reports zero in flight for a
// session that never produces a single observation (a pure conversation,
// no tool calls) — must NOT have its budget extended to
// stopInFlightWaitMaxAttempts (five minutes) just because the worker
// happened to be reachable. It should still give up at exactly
// stopWaitMaxAttempts, matching the pre-existing, already-shipped
// "genuinely empty" behavior this test guards against silently
// regressing into a much longer wait.
func TestWaitForSessionObservationsGenuinelyEmptySessionStillBoundedEvenWhenWorkerReachable(t *testing.T) {
	setFastPollIntervalForTest(t)

	be := &sequencedBackend{counts: []int{0}} // sequencedBackend repeats the last entry forever
	alwaysIdle := func(sessionID string) (int, bool) { return 0, true }

	got, err := waitForSessionObservations(be, "s1", 50, alwaysIdle)
	if err != nil {
		t.Fatalf("waitForSessionObservations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("waitForSessionObservations returned %d observations, want 0", len(got))
	}
	if be.calls != stopWaitMaxAttempts {
		t.Fatalf("waitForSessionObservations made %d BySessionID calls, want exactly %d — a genuinely empty session must not get the extended budget just because the worker was reachable", be.calls, stopWaitMaxAttempts)
	}
}
