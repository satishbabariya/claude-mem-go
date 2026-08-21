package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"time"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/observer"
	"claude-mem-go/store"
)

// stopWaitPollInterval and stopWaitMaxAttempts bound waitForSessionObservations's
// polling (see its own doc comment for the real race this closes).
// stopWaitPollInterval is a var, not a const, so tests can shrink it to
// run the same iteration-count logic near-instantly rather than actually
// sleeping for real seconds per test case.
var stopWaitPollInterval = 1 * time.Second

const stopWaitMaxAttempts = 12 // ~12s worst case at the real interval, with margin over the real ~8s observer latency this was found against

// waitForSessionObservations polls BySessionID until the count stops
// growing across two consecutive checks (the worker's own async
// PostToolUse pipeline has caught up), or stopWaitMaxAttempts is reached
// — whichever comes first.
//
// A real bug found by hand, not anticipated: Stop fires the instant the
// user's session ends, but PostToolUse is fire-and-forget — the actual
// observation for the session's LAST tool call is a real LLM call the
// worker daemon runs in the background, taking several real seconds. A
// single, immediate BySessionID call can race ahead of it: reproduced
// directly, one real session's Stop hook fired and found ZERO
// observations (nothing to summarize) 4 seconds before the worker
// finished persisting the one observation that session actually had. In
// a longer session with multiple tool calls, the identical race would
// instead silently produce a summary missing just its most recent,
// often most contextually important, action — with nothing anywhere
// indicating anything was missed.
//
// Safe to poll rather than accept whatever the first check finds: Stop
// itself runs fire-and-forget (hooks.json marks it "async": true, same
// as PostToolUse) — nothing is waiting on this process, so the extra
// wait costs real wall-clock time in a background process, not perceived
// latency for the user waiting on their own session to end.
//
// This narrows the race; it doesn't eliminate every variant of it. There
// is no way for this hook to know for certain "have ALL PostToolUse
// events for this session finished," only to infer it from the count no
// longer growing across a short window — a worker under sustained heavy
// load could still occasionally miss the last event even with this fix.
//
// Deliberately does NOT treat two consecutive zero-reads as "stabilized
// at zero, genuinely nothing to summarize" — caught live, not just in a
// unit test: a session with exactly one real tool call reads 0 on every
// check until the worker's own observer call actually finishes, which
// routinely took longer than one poll interval. Two zero-reads 1s apart
// prove "not yet," never "never" — only a count that has gone positive
// and THEN stops growing is real evidence of stability. A session that
// genuinely never produces any observation (a pure conversation, no
// tool calls at all) pays the full wait budget before this gives up —
// an acceptable cost since Stop runs fire-and-forget, not a cost the
// user waiting on their own session ever sees.
func waitForSessionObservations(st store.Backend, sessionID string, limit int) ([]store.SearchResult, error) {
	var observations []store.SearchResult
	prevCount := -1
	for attempt := 0; attempt < stopWaitMaxAttempts; attempt++ {
		obs, err := st.BySessionID(sessionID, limit)
		if err != nil {
			return nil, err
		}
		observations = obs
		if len(obs) > 0 && len(obs) == prevCount {
			return observations, nil
		}
		prevCount = len(obs)
		time.Sleep(stopWaitPollInterval)
	}
	return observations, nil
}

// cmdStop is the Stop hook: real claude-mem's "summarize" step,
// reimplemented from what's already persisted per tool call rather than
// re-reading the raw transcript. Fire-and-forget like PostToolUse (real
// hooks.json marks Stop "async": true too) — nothing reads this process's
// stdout, so diagnostics go to a log file and that's the only output.
func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	limit := fs.Int("limit", 50, "max observations from this session to include in the summary")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(empty to skip embedding — the summary is still persisted, just not semantically searchable)")
	fs.Parse(args)
	*limit = clampLimit(*limit, 50, 100)

	l := openLog("stop.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Printf("FAILED parsing hook payload: %v", err)
		return 0
	}
	if in.SessionID == "" {
		l.Printf("no session_id in Stop payload, skipping")
		return 0
	}

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		l.Printf("FAILED opening store at %s: %v", store.RedactDSN(*dbPath), err)
		return 0
	}
	defer st.Close()

	observations, err := waitForSessionObservations(st, in.SessionID, *limit)
	if err != nil {
		l.Printf("FAILED BySessionID(%s): %v", in.SessionID, err)
		return 0
	}
	if len(observations) == 0 {
		l.Printf("no observations for session=%s, nothing to summarize", in.SessionID)
		return 0
	}

	// Idempotency key is the session_id alone, not what's being summarized —
	// exactly one summary per session regardless of how many times Stop
	// fires or how the observation count changes between firings.
	hash := store.ContentHash(in.SessionID, "SessionSummary", "session-summary", "")

	project := filepath.Base(in.Cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(in.TranscriptPath))
	}

	obs, err := observer.New(context.Background(), *model)
	if err != nil {
		l.Printf("FAILED to start observer: %v", err)
		return 0
	}
	defer obs.Close()

	summaryTurn, err := obs.Summarize(observations)
	if err != nil {
		l.Printf("FAILED summarizing session %s: %v", in.SessionID, err)
		return 0
	}

	res, err := st.Insert(in.SessionID, project, "SessionSummary", hash, summaryTurn.Observation, summaryTurn.Result.CostUSD)
	if err != nil {
		l.Printf("FAILED sqlite insert: %v", err)
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
		l.Printf("embedding failed for observations.id=%d (semantic search won't find it): %v", res.ID, err)
		return 0
	}
	if err := st.SaveEmbedding(res.ID, vec); err != nil {
		l.Printf("saving embedding for observations.id=%d failed: %v", res.ID, err)
	}
	return 0
}
