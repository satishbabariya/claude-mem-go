package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"time"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/excludeproject"
	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/worker"
)

// stopWaitPollInterval, stopStableStreakRequired, and stopWaitMaxAttempts
// bound waitForSessionObservations's polling (see its own doc comment for
// the real races this closes — there have been two). stopWaitPollInterval
// is a var, not a const, so tests can shrink it to run the same
// iteration-count logic near-instantly rather than actually sleeping for
// real seconds per test case.
var stopWaitPollInterval = 1 * time.Second

const (
	// stopStableStreakRequired is how many CONSECUTIVE matching non-zero
	// reads are needed before trusting the count as final — i.e., roughly
	// this many seconds of confirmed no-growth. Sized well above a single
	// observer call's own real latency (~5-8s, confirmed by hand against
	// real sessions), not just "a couple of quick polls": a session with
	// multiple tool calls processes them SEQUENTIALLY (one worker mutex
	// serializes turns per session, see sessions.go), so the count can sit
	// at N for several real seconds — long enough to look "stable" by a
	// short window — while the NEXT observation is still mid-flight and
	// about to become N+1.
	stopStableStreakRequired = 10
	// stopWaitMaxAttempts bounds the total wait regardless of streak
	// progress — a hard ceiling so a pathological or perpetually-growing
	// session (or one with no tool calls at all, which never gets to
	// start a streak) can't hang this hook forever. Large enough to
	// comfortably outlast stopStableStreakRequired's own confirmation
	// window plus room for a few real observer calls ahead of it.
	stopWaitMaxAttempts = 45
	// stopInFlightConfirmStreak is the (much shorter) confirmation window
	// used when the worker daemon itself reports zero in-flight events for
	// this session — see waitForSessionObservations. A short debounce, not
	// a guess at observer latency like stopStableStreakRequired: this is
	// asking the one process that actually knows, not inferring from a row
	// count, so it only needs to guard against a query landing in the
	// narrow gap right as an in-flight count transitions, not against the
	// full observer latency the row-count heuristic has to out-wait.
	stopInFlightConfirmStreak = 2
	// stopInFlightWaitMaxAttempts replaces stopWaitMaxAttempts as the wait
	// ceiling once the worker has confirmed real activity for this session
	// (an observation already persisted, or an event actively in flight) —
	// see waitForSessionObservations. Much larger than stopWaitMaxAttempts
	// because, once the worker is reachable, waiting longer is no longer a
	// guess: a live re-verification run this session hit a real, single
	// observation that took 104 seconds — comfortably longer than
	// stopWaitMaxAttempts's ~45s ceiling — because it involved an
	// unusually large summarization call. Sized generously past that.
	// Never applies to a genuinely tool-call-free session (see
	// waitForSessionObservations: the extension requires having actually
	// seen activity), so a pure-conversation session's Stop hook still
	// finishes in ~stopWaitMaxAttempts seconds, not five minutes.
	stopInFlightWaitMaxAttempts = 300
)

// waitForSessionObservations polls BySessionID until the count holds
// steady for stopStableStreakRequired consecutive checks (the worker's
// own async PostToolUse pipeline has caught up), or stopWaitMaxAttempts
// is reached — whichever comes first.
//
// inFlight, when non-nil, is queried on every attempt (typically
// hook.QueryInFlight against the real worker daemon) and changes waiting
// behavior two ways, both only once the worker actually confirms real
// activity for this session (an observation already persisted, or an
// event it reports actively in flight) — a session that never shows any
// activity at all still just pays the original stopWaitMaxAttempts budget
// below, unchanged:
//
//  1. Faster exit: once the worker reports zero events in flight for
//     stopInFlightConfirmStreak consecutive checks, this returns
//     immediately rather than waiting out the full row-count streak.
//  2. Longer patience: the wait ceiling extends from stopWaitMaxAttempts
//     to the much larger stopInFlightWaitMaxAttempts, because once the
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
// (stopStableStreakRequired) closes that gap in practice by demanding
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
func waitForSessionObservations(ctx context.Context, st memory.Backend, sessionID string, limit int, inFlight func(sessionID string) (int, bool)) ([]memory.SearchResult, error) {
	var observations []memory.SearchResult
	prevCount := -1
	streak := 0
	inFlightZeroStreak := 0
	maxAttempts := stopWaitMaxAttempts
	// sawActivity gates the ceiling extension below: only real evidence
	// that this session has (or had) a turn actually happening — not just
	// "the worker answered our query" — earns the longer budget. Without
	// this, a genuinely tool-call-free session (a pure conversation, never
	// any activity at all) would pay the full five-minute ceiling just
	// because the worker happened to be reachable, not because there was
	// ever anything to wait for.
	sawActivity := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		obs, err := st.BySessionID(ctx, sessionID, limit)
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
					if inFlightZeroStreak >= stopInFlightConfirmStreak {
						return observations, nil
					}
				} else {
					inFlightZeroStreak = 0
				}
				if sawActivity && maxAttempts < stopInFlightWaitMaxAttempts {
					maxAttempts = stopInFlightWaitMaxAttempts
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
			// stopStableStreakRequired and return early purely because a
			// row count happened to hold steady, even while the worker was
			// simultaneously reporting real ongoing work — exactly the
			// plateau failure mode stopStableStreakRequired exists to
			// guard against, now closed with a real signal instead of a
			// guessed threshold, but only if it's allowed to win.
			streak = 0
		} else if len(obs) > 0 && len(obs) == prevCount {
			streak++
			if streak >= stopStableStreakRequired {
				return observations, nil
			}
		} else {
			streak = 0
		}
		prevCount = len(obs)
		time.Sleep(stopWaitPollInterval)
	}
	return observations, nil
}

// summaryFetchCap bounds the re-read cmdStop does to learn a session's
// TRUE observation count before choosing a summary window. Deliberately
// far above the -limit ceiling of 100: it is a backstop against a
// pathological session, not the summarization budget. At 2000 rows of
// (id, title, subtitle, type) this is a few hundred KB and one indexed
// query, paid once per session end.
const summaryFetchCap = 2000

// cmdStop is the Stop hook: real claude-mem's "summarize" step,
// reimplemented from what's already persisted per tool call rather than
// re-reading the raw transcript. Fire-and-forget like PostToolUse (real
// hooks.json marks Stop "async": true too) — nothing reads this process's
// stdout, so diagnostics go to a log file and that's the only output.
func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	dbPath := fs.String("db", memory.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "worker daemon's unix socket, queried for real in-flight state (best-effort — falls back to a row-count heuristic if unreachable)")
	limit := fs.Int("limit", 50, "max observations from this session to include in the summary")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(empty to skip embedding — the summary is still persisted, just not semantically searchable)")
	excludedProjects := fs.String("excluded-projects", "", "comma-separated glob patterns (supports *, **, ?, and a leading ~) — "+
		"a matching project gets no automatic session summary, the real claude-mem CLAUDE_MEM_EXCLUDED_PROJECTS feature; "+
		"empty (the default) excludes nothing")
	fs.Parse(args)
	*limit = clampLimit(*limit, 50, 100)

	l := openLog("stop.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Errorf("FAILED parsing hook payload: %v", err)
		return 0
	}
	if in.SessionID == "" {
		l.Printf("no session_id in Stop payload, skipping")
		return 0
	}
	if excludeproject.IsExcluded(in.Cwd, *excludedProjects) {
		l.Printf("skip: project excluded (cwd=%s)", in.Cwd)
		return 0
	}
	if in.AgentID != "" || in.AgentType != "" {
		l.Printf("skip: subagent context detected (agent_id=%s agent_type=%s)", in.AgentID, in.AgentType)
		return 0
	}
	// Claude Code asks for this one explicitly. When a Stop hook blocks a
	// turn from ending, the turn is retried and every Stop hook fires
	// again with stop_hook_active=true; on hitting the cap Claude Code
	// prints "For Stop/SubagentStop hooks, check stop_hook_active in the
	// input and return success while it's true." Real claude-mem's own
	// summarize handler makes the same check first thing.
	//
	// This hook does not block, so it never causes the retry itself — but
	// it is dragged along by any OTHER blocking Stop hook in the user's
	// setup, and the cost of being dragged along was measured rather than
	// assumed: a repeat Stop on an already-summarized session took **17.3
	// seconds** and made a real, billed observer call before the
	// deterministic content hash finally rejected the insert. At the
	// default cap of 8 (CLAUDE_CODE_STOP_HOOK_BLOCK_CAP) that is roughly
	// two and a half minutes of added latency and eight wasted model calls
	// per turn, repeated for every turn of the session.
	//
	// Returning success is exactly what Claude Code asks for and loses
	// nothing: the summary for this session either already exists (the
	// retry case) or will be written when the turn genuinely ends.
	if in.StopHookActive {
		l.Printf("skip: stop_hook_active — this turn is being retried because a Stop hook blocked it; "+
			"session %s will be summarized when the turn actually ends", in.SessionID)
		return 0
	}
	// Real claude-mem's own PrivacyCheckValidator makes this same check
	// before generating a Stop-time summary (SessionRoutes.ts), not just
	// before a PostToolUse observation — a turn a user marked entirely
	// private (see prompt-context and worker.process's identical check)
	// must not surface in the session summary either. Stop runs as its own
	// process with no direct access to the worker's in-memory flag, hence
	// the socket query rather than a direct call. A query failure (daemon
	// unreachable) deliberately falls through to summarizing normally
	// rather than skipping — an unknown signal must never be treated as
	// "private," the same reasoning d.sessions.isPrivate's own doc comment
	// gives for why an absent flag defaults to false.
	if private, err := hook.QueryPrivate(*socketPath, in.SessionID); err == nil && private {
		l.Printf("skip: session %s marked private for this turn", in.SessionID)
		return 0
	}

	ctx, cancel := hookContext(stopBudget)
	defer cancel()
	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		l.Errorf("FAILED opening store at %s: %v", memory.RedactDSN(*dbPath), err)
		return 0
	}
	defer st.Close()

	// Bail before the wait budget AND before the model call if this
	// session already has its summary.
	//
	// The insert at the end is already idempotent — the content hash is
	// derived from the session id alone, not from the model's output, so a
	// second summary row is impossible. But that check happens LAST, after
	// waiting for observations to settle and after paying for a real
	// observer call whose result is then discarded. Measured on a repeat
	// Stop: 17.3 seconds and one billed call, to produce nothing.
	//
	// stop_hook_active (above) catches the specific case Claude Code warns
	// about. This catches every other way Stop runs twice for one session
	// — a manually re-fired hook, a crash-and-retry, a session resumed
	// after its summary was already written — without needing to enumerate
	// them. It costs one extra query on the normal path, where it finds
	// nothing and falls through.
	if existing, err := st.BySessionID(ctx, in.SessionID, *limit); err == nil {
		for _, o := range existing {
			if o.Observation.Type == "summary" {
				l.Printf("skip: session %s already summarized (observations.id=%d) — "+
					"not re-running the observer for a result the content hash would reject", in.SessionID, o.ID)
				return 0
			}
		}
	}

	inFlight := func(sessionID string) (int, bool) {
		n, err := hook.QueryInFlight(*socketPath, sessionID)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	observations, err := waitForSessionObservations(ctx, st, in.SessionID, *limit, inFlight)
	if err != nil {
		l.Errorf("FAILED BySessionID(%s): %v", in.SessionID, err)
		return 0
	}
	if len(observations) == 0 {
		l.Printf("no observations for session=%s, nothing to summarize", in.SessionID)
		return 0
	}

	// Idempotency key is the session_id alone, not what's being summarized —
	// exactly one summary per session regardless of how many times Stop
	// fires or how the observation count changes between firings.
	hash := memory.ContentHash(in.SessionID, "SessionSummary", "session-summary", "")

	project := memory.ProjectFor(in.Cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(in.TranscriptPath))
	}

	// `observations` came back capped at *limit, which is where the
	// summary's real defect lived: BySessionID orders oldest-first, so a
	// plain LIMIT kept the FIRST *limit observations and dropped
	// everything after them — the end of the session, which is where its
	// conclusions are. Measured on a real 150-observation session at the
	// default cap of 50: the summary described routine early edits, said
	// "an extensive series of 50 sequential edits", and contained no trace
	// of the decision recorded 50 times in the tail.
	//
	// So re-read the session without the summarization cap, then choose a
	// window deliberately (head + tail, middle elided) instead of letting
	// SQL's LIMIT choose it. summaryFetchCap still bounds the read — an
	// unbounded query on a pathological session is its own problem — but
	// it is far above *limit, so Total is the true count in every
	// realistic case and the prompt can say so.
	all := observations
	if full, ferr := st.BySessionID(ctx, in.SessionID, summaryFetchCap); ferr != nil {
		l.Warnf("full re-read for windowing failed, falling back to the capped set: %v", ferr)
	} else if len(full) > len(all) {
		all = full
	}
	window := observer.SelectSummaryWindow(all, *limit)
	if window.Total > len(window.Observations) {
		l.Printf("session %s has %d observations; summarizing from %d (earliest + most recent, middle elided)",
			in.SessionID, window.Total, len(window.Observations))
	}

	obs, err := observer.New(ctx, *model)
	if err != nil {
		l.Errorf("FAILED to start observer: %v", err)
		return 0
	}
	defer obs.Close()

	summaryTurn, err := obs.Summarize(window)
	if err != nil {
		l.Errorf("FAILED summarizing session %s: %v", in.SessionID, err)
		return 0
	}

	res, err := st.Insert(ctx, in.SessionID, project, "SessionSummary", hash, summaryTurn.Observation, summaryTurn.Result.CostUSD)
	if err != nil {
		l.Errorf("FAILED sqlite insert: %v", err)
		return 0
	}
	if !res.Inserted {
		l.Printf("session %s already summarized (observations.id=%d)", in.SessionID, res.ID)
		return 0
	}
	l.Printf("persisted session summary observations.id=%d title=%q from %d observations, cost=$%.4f",
		res.ID, summaryTurn.Observation.Title, len(observations), summaryTurn.Result.CostUSD)

	// Additive only, same as the worker's own embedding step: keyword
	// search on the summary just inserted already works without this, and
	// a missing/unreachable Ollama must not undo a successful summary.
	// Without this, a session summary — arguably the single most
	// information-dense observation this project ever produces — was
	// invisible to semantic_search_observations from the day this hook
	// was written, findable only by keyword search or by listing.
	if *embedModel == "" {
		return 0
	}
	text := embed.ObservationText(summaryTurn.Observation.Title, summaryTurn.Observation.Subtitle,
		summaryTurn.Observation.Narrative, summaryTurn.Observation.Facts)
	vec, err := embed.NewClient(*embedModel).Embed(text)
	if err != nil {
		l.Warnf("embedding failed for observations.id=%d (semantic search won't find it): %v", res.ID, err)
		return 0
	}
	if err := st.SaveEmbedding(ctx, res.ID, vec); err != nil {
		l.Warnf("saving embedding for observations.id=%d failed: %v", res.ID, err)
	}
	return 0
}
