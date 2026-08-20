package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/observer"
	"claude-mem-go/store"
)

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

	observations, err := st.BySessionID(in.SessionID, *limit)
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
