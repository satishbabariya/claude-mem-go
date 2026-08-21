package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/store"
)

// cmdReembed is the remediation half of doctor's embedding_dims_consistent
// finding: detecting a stale/inconsistent embedding (e.g. after the
// configured Ollama model changed) is one thing, but there was no way to
// actually FIX it short of re-ingesting from scratch. Also naturally
// catches observations that were simply never embedded at all — the more
// common real case (no embed model configured at capture time, or Ollama
// unreachable that day) — since ObservationsNeedingEmbedding treats
// "never embedded" and "embedded with the wrong dimension" as the same
// condition.
//
// Dry-run by default, like prune: this costs one real Ollama API call per
// affected row, so a caller should see the count before committing to
// however many of those it implies. Not destructive in prune's sense
// (nothing is ever deleted), but real API cost is its own reason to look
// before doing it.
func cmdReembed(args []string) int {
	fs := flag.NewFlagSet("reembed", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model to re-embed with")
	project := fs.String("project", "", "scope to one project (default: every project in the store)")
	yes := fs.Bool("yes", false, "actually re-embed — without this, reembed only reports how many rows WOULD be re-embedded")
	fs.Parse(args)

	client := embed.NewClient(*embedModel)
	// The current model's real dimension count, learned from the model
	// itself rather than hardcoded — nothing in this codebase maintains a
	// model-name-to-dimension lookup table, and a probe embed call is the
	// only way to know for certain what THIS model actually produces
	// right now.
	probe, err := client.Embed("dimension probe")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to reach Ollama with model %q: %v\n", *embedModel, err)
		return 1
	}
	expectedDims := int64(len(probe))

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	const pageSize = 100
	var afterID int64
	var candidates, reembedded, failed int
	for {
		batch, err := st.ObservationsNeedingEmbedding(*project, expectedDims, afterID, pageSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED querying observations needing embedding: %v\n", err)
			return 1
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			candidates++
			afterID = r.ID
			if !*yes {
				continue
			}
			text := embed.ObservationText(r.Observation.Title, r.Observation.Subtitle, r.Observation.Narrative, r.Observation.Facts)
			vec, err := client.Embed(text)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAILED embedding observation id=%d: %v\n", r.ID, err)
				failed++
				continue
			}
			if err := st.SaveEmbedding(r.ID, vec); err != nil {
				fmt.Fprintf(os.Stderr, "FAILED saving embedding for observation id=%d: %v\n", r.ID, err)
				failed++
				continue
			}
			reembedded++
		}
		if len(batch) < pageSize {
			break
		}
	}

	scope := "every project"
	if *project != "" {
		scope = fmt.Sprintf("project %q", *project)
	}
	if !*yes {
		fmt.Printf("%d observation(s) in %s need embedding with model %q (%d dims). Re-run with -yes to actually re-embed them.\n", candidates, scope, *embedModel, expectedDims)
		return 0
	}
	fmt.Printf("Re-embedded %d/%d observation(s) in %s with model %q (%d dims)", reembedded, candidates, scope, *embedModel, expectedDims)
	if failed > 0 {
		fmt.Printf(" — %d failed (see stderr above)", failed)
	}
	fmt.Println()
	if failed > 0 {
		return 1
	}
	return 0
}
