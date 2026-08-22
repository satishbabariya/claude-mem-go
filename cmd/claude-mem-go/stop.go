package main

import (
	"flag"
	"os"
	"path/filepath"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/excludeproject"
	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/worker"
)

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
	dbPath := cli.DBFlag(fs)
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "worker daemon's unix socket, queried for real in-flight state (best-effort — falls back to a row-count heuristic if unreachable)")
	limit := fs.Int("limit", 50, "max observations from this session to include in the summary")
	embedModel := fs.String("embed-model", cli.DefaultEmbedModel, "Ollama model for embeddings "+
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
	observations, err := cli.WaitForSessionObservations(ctx, st, in.SessionID, *limit, inFlight)
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
