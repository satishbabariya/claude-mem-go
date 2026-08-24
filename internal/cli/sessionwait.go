// Package cli holds the pieces of the claude-mem-go command that are
// logic rather than wiring: a rotating log writer, the flag definitions
// every subcommand shares, and the Stop hook's wait-for-the-worker loop.
// They live here, not in package main, so they can be imported and tested
// without the binary's dispatch table.
package cli

import (
	"context"
	"errors"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// StopWaitPollInterval, StopStableStreakRequired, and StopWaitMaxAttempts
// bound WaitForSessionObservations's polling (see its own doc comment for
// the real races this closes — there have been two). StopWaitPollInterval
// is a var, not a const, so tests can shrink it to run the same
// iteration-count logic near-instantly rather than actually sleeping for
// real seconds per test case.
var StopWaitPollInterval = 1 * time.Second

const (
	// StopStableStreakRequired is how many CONSECUTIVE matching non-zero
	// reads are needed before trusting the count as final — i.e., roughly
	// this many seconds of confirmed no-growth. Sized well above a single
	// observer call's own real latency (~5-8s, confirmed by hand against
	// real sessions), not just "a couple of quick polls": a session with
	// multiple tool calls processes them SEQUENTIALLY (one worker mutex
	// serializes turns per session, see sessions.go), so the count can sit
	// at N for several real seconds — long enough to look "stable" by a
	// short window — while the NEXT observation is still mid-flight and
	// about to become N+1.
	StopStableStreakRequired = 10
	// StopWaitMaxAttempts bounds the total wait regardless of streak
	// progress — a hard ceiling so a pathological or perpetually-growing
	// session (or one with no tool calls at all, which never gets to
	// start a streak) can't hang this hook forever. Large enough to
	// comfortably outlast StopStableStreakRequired's own confirmation
	// window plus room for a few real observer calls ahead of it.
	StopWaitMaxAttempts = 45
	// StopInFlightConfirmStreak is the (much shorter) confirmation window
	// used when the worker daemon itself reports zero in-flight events for
	// this session — see WaitForSessionObservations. A short debounce, not
	// a guess at observer latency like StopStableStreakRequired: this is
	// asking the one process that actually knows, not inferring from a row
	// count, so it only needs to guard against a query landing in the
	// narrow gap right as an in-flight count transitions, not against the
	// full observer latency the row-count heuristic has to out-wait.
	StopInFlightConfirmStreak = 2
	// StopInFlightWaitMaxAttempts replaces StopWaitMaxAttempts as the wait
	// ceiling once the worker has confirmed real activity for this session
	// (an observation already persisted, or an event actively in flight) —
	// see WaitForSessionObservations. Much larger than StopWaitMaxAttempts
	// because, once the worker is reachable, waiting longer is no longer a
	// guess: a live re-verification run this session hit a real, single
	// observation that took 104 seconds — comfortably longer than
	// StopWaitMaxAttempts's ~45s ceiling — because it involved an
	// unusually large summarization call. Sized generously past that.
	// Never applies to a genuinely tool-call-free session (see
	// WaitForSessionObservations: the extension requires having actually
	// seen activity), so a pure-conversation session's Stop hook still
	// finishes in ~StopWaitMaxAttempts seconds, not five minutes.
	StopInFlightWaitMaxAttempts = 300
)

// StopSummarizeReserve is how much of the caller's context budget
// WaitForSessionObservations refuses to spend, so there is time left to
// run the observer and insert the summary. Sized from measured Stop-hook
// summaries, which take roughly 10-30s end to end.
const StopSummarizeReserve = 45 * time.Second

// ErrWaitBudgetExhausted means the wait stopped early to leave time for
// summarizing, not that anything failed. Callers should summarize the
// observations returned alongside it rather than treating it as an error.
var ErrWaitBudgetExhausted = errors.New("stopped waiting for in-flight observations to leave time to summarize")

// WaitForSessionObservations polls BySessionID until the count holds
// steady for StopStableStreakRequired consecutive checks (the worker's
// own async PostToolUse pipeline has caught up), or StopWaitMaxAttempts
// is reached — whichever comes first.
//
// inFlight, when non-nil, is queried on every attempt (typically
// hook.QueryInFlight against the real worker daemon) and changes waiting
// behavior two ways, both only once the worker actually confirms real
// activity for this session (an observation already persisted, or an
// event it reports actively in flight) — a session that never shows any
// activity at all still just pays the original StopWaitMaxAttempts budget
// below, unchanged:
//
//  1. Faster exit: once the worker reports zero events in flight for
//     StopInFlightConfirmStreak consecutive checks, this returns
//     immediately rather than waiting out the full row-count streak.
//  2. Longer patience: the wait ceiling extends from StopWaitMaxAttempts
//     to the much larger StopInFlightWaitMaxAttempts, because once the
//     worker is reachable, waiting longer is no longer a blind guess — a
//     live re-verification run this session hit a real observation that
//     took 104 seconds, longer than the original ~45s ceiling, and would
//     have been cut off mid-flight without this.
//
// A query's second return value is whether it succeeded — a failure
// (worker not running, wrong socket path, crashed) must fall back to the
// row-count heuristic below entirely, not be treated as "confirmed zero."
// This closes the exact architectural gap this function's own doc comment
// used to name as unresolved: previously there was no way to ask the
// worker directly whether it was still catching up on a session, only to
// infer it from watching the observations table, a heuristic that (see
// below) a sequential-processing plateau could fool, and which had no way
// to distinguish "still working, keep waiting" from "give up now" beyond
// a fixed timeout. It's still not a perfect signal — see
// hook.QueryInFlight's own doc comment — but it's authoritative rather
// than inferred, which the row-count check never was.
//
// A real bug found by hand, not anticipated: Stop fires the instant the
// user's session ends, but PostToolUse is fire-and-forget — each
// observation is a real LLM call the worker daemon runs in the
// background, taking several real seconds. A single, immediate
// BySessionID call can race ahead of it: reproduced directly, one real
// session's Stop hook fired and found ZERO observations (nothing to
// summarize) 4 seconds before the worker finished persisting the one
// observation that session actually had.
//
// A second real bug, found the same way against a session with TWO real
// tool calls: this function's first version required only two
// consecutive matching reads to trust stability, and locked in at count=1
// (just the first tool call's observation) while the second one — for a
// DIFFERENT tool call in the same session, still queued behind the
// first because they're processed sequentially — was still several
// seconds away from landing. The resulting summary silently covered only
// part of the session. Requiring a much longer stable streak
// (StopStableStreakRequired) closes that gap in practice by demanding
// confirmed quiet time comfortably longer than one observation's own
// real latency, though it's still a heuristic, not a guarantee — see
// below.
//
// Safe to poll rather than accept whatever the first check finds: Stop
// itself runs fire-and-forget (hooks.json marks it "async": true, same
// as PostToolUse) — nothing is waiting on this process, so the extra
// wait costs real wall-clock time in a background process, not perceived
// latency for the user waiting on their own session to end.
//
// This narrows both races; it doesn't eliminate every variant of them.
// There is no way for this hook to know for certain "have ALL
// PostToolUse events for this session finished" — only to infer it from
// the count holding steady across a long-enough window. A session with
// many tool calls queued back to back, or a worker under sustained heavy
// load, could in principle still exceed even this margin. A fully robust
// fix would need the worker daemon to expose real per-session "is a turn
// for this session currently in flight" state for this hook to query
// directly, rather than inferring it from watching the result table —
// a real architectural change, not a parameter to tune, and a
// legitimate follow-up rather than something this fix attempts.
//
// Deliberately does NOT treat matching zero-reads as "stabilized at
// zero, genuinely nothing to summarize" — caught live, not just in a
// unit test: a session with a real tool call reads 0 on every check
// until the worker's own observer call actually finishes. Zero-reads
// prove "not yet," never "never" — only a count that has gone positive
// and THEN holds the required streak is treated as real evidence. A
// session that genuinely never produces any observation (a pure
// conversation, no tool calls at all) pays the full wait budget before
// this gives up — an acceptable cost since Stop runs fire-and-forget,
// not a cost the user waiting on their own session ever sees.
//
// project is passed straight through to BySessionID (empty = every
// project); the Stop hook passes the project the worker recorded under.
func WaitForSessionObservations(ctx context.Context, st memory.Backend, project, sessionID string, limit int, inFlight func(sessionID string) (int, bool)) ([]memory.SearchResult, error) {
	var observations []memory.SearchResult
	prevCount := -1
	streak := 0
	inFlightZeroStreak := 0
	maxAttempts := StopWaitMaxAttempts
	// sawActivity gates the ceiling extension below: only real evidence
	// that this session has (or had) a turn actually happening — not just
	// "the worker answered our query" — earns the longer budget. Without
	// this, a genuinely tool-call-free session (a pure conversation, never
	// any activity at all) would pay the full five-minute ceiling just
	// because the worker happened to be reachable, not because there was
	// ever anything to wait for.
	sawActivity := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Stop waiting while there is still budget left to summarize with.
		// StopInFlightWaitMaxAttempts (300s) is larger than the Stop hook's
		// own context budget (110s, see cmd/claude-mem-go/ctx.go), so a
		// session whose observations never settle used to poll straight
		// through the deadline: BySessionID then failed with "context
		// deadline exceeded", which the caller logged at ERROR, and no
		// summary was written at all. Observed live on 2026-08-24. The
		// wait now yields early, and the caller summarizes whatever was
		// recorded — a partial summary beats an error and nothing.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= StopSummarizeReserve {
			return observations, ErrWaitBudgetExhausted
		}
		obs, err := st.BySessionID(ctx, project, sessionID, limit)
		if err != nil {
			return nil, err
		}
		observations = obs
		if len(obs) > 0 {
			sawActivity = true
		}

		workerConfirmedBusy := false
		if inFlight != nil {
			if n, ok := inFlight(sessionID); ok {
				if n > 0 {
					sawActivity = true
					workerConfirmedBusy = true
				}
				if n == 0 && len(obs) > 0 {
					inFlightZeroStreak++
					if inFlightZeroStreak >= StopInFlightConfirmStreak {
						return observations, nil
					}
				} else {
					inFlightZeroStreak = 0
				}
				if sawActivity && maxAttempts < StopInFlightWaitMaxAttempts {
					maxAttempts = StopInFlightWaitMaxAttempts
				}
			} else {
				inFlightZeroStreak = 0
			}
		}

		if workerConfirmedBusy {
			// The worker just told us directly that something is still in
			// flight for this session — that overrides any illusion of
			// stability the row count alone might otherwise suggest. Without
			// this override, the row-count streak below could still reach
			// StopStableStreakRequired and return early purely because a
			// row count happened to hold steady, even while the worker was
			// simultaneously reporting real ongoing work — exactly the
			// plateau failure mode StopStableStreakRequired exists to
			// guard against, now closed with a real signal instead of a
			// guessed threshold, but only if it's allowed to win.
			streak = 0
		} else if len(obs) > 0 && len(obs) == prevCount {
			streak++
			if streak >= StopStableStreakRequired {
				return observations, nil
			}
		} else {
			streak = 0
		}
		prevCount = len(obs)
		time.Sleep(StopWaitPollInterval)
	}
	return observations, nil
}
