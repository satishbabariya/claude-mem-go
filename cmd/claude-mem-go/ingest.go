package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/transcript"
)

func cmdIngest(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	dbPath := cli.DBFlag(fs)
	transcriptPath := fs.String("transcript", "", "transcript .jsonl path; "+
		"defaults to the most recently modified one under ~/.claude/projects/*/*.jsonl")
	limit := fs.Int("limit", 3, "how many real tool_use/tool_result pairs to ingest")
	embedModel := fs.String("embed-model", cli.DefaultEmbedModel, "Ollama model for embeddings "+
		"(empty to skip embedding — observations are still persisted, just not semantically searchable)")
	fs.Parse(args)
	*limit = clampLimit(*limit, 3, 100)

	tp := *transcriptPath
	if tp == "" {
		found, err := transcript.FindMostRecent()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED to find a transcript: %v\n", err)
			return 1
		}
		tp = found
	}
	fmt.Printf("transcript: %s\n", tp)

	calls, err := transcript.Parse(tp, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to parse transcript: %v\n", err)
		return 1
	}
	if len(calls) == 0 {
		fmt.Fprintf(os.Stderr, "FAILED: found zero tool_use/tool_result pairs in %s\n", tp)
		return 1
	}
	fmt.Printf("ingested %d real tool_use/tool_result pairs\n\n", len(calls))

	ctx, cancel := cliContext()
	defer cancel()
	obs, err := observer.New(ctx, *model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to start observer: %v\n", err)
		return 1
	}
	defer obs.Close()

	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	project := filepath.Base(filepath.Dir(tp))

	for i, tc := range calls {
		turn, err := observer.ObserveResilient(ctx, obs, *model, tc, observer.DefaultRetryPolicy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "turn %d FAILED: %v\n", i+1, err)
			return 1
		}
		fmt.Printf("turn %d: session=%s cost=$%.4f title=%q\n",
			i+1, turn.Result.SessionID, turn.Result.CostUSD, turn.Observation.Title)

		hash := memory.ContentHash(turn.Result.SessionID, tc.ToolName, tc.ToolInput, tc.ToolOutput)
		res, err := st.Insert(ctx, turn.Result.SessionID, project, tc.ToolName, hash, turn.Observation, turn.Result.CostUSD)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: sqlite insert failed: %v\n", err)
			continue
		}
		if !res.Inserted {
			fmt.Printf("  -> already ingested as observations.id=%d (same tool call, skipped duplicate)\n", res.ID)
			continue
		}
		fmt.Printf("  -> persisted as observations.id=%d\n", res.ID)

		if *embedModel == "" {
			continue
		}
		text := embed.ObservationText(turn.Observation.Title, turn.Observation.Subtitle,
			turn.Observation.Narrative, turn.Observation.Facts)
		vec, err := embed.NewClient(*embedModel).Embed(ctx, text)
		if err != nil {
			// Embedding is additive — keyword search (already persisted above)
			// still works without it. A missing/unreachable Ollama must not
			// fail the whole ingest.
			fmt.Fprintf(os.Stderr, "  WARNING: embedding failed, semantic search won't find this one: %v\n", err)
			continue
		}
		if err := st.SaveEmbedding(ctx, res.ID, vec); err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: saving embedding failed: %v\n", err)
			continue
		}
		fmt.Printf("  -> embedded (%d dims)\n", len(vec))
	}
	return 0
}
