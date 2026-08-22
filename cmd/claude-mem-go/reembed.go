package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
)

// maxConsecutiveFailures stops a run once the embedding service is
// clearly down, rather than working through every remaining row.
//
// The old loop did `failed++; continue` forever, and the cost of that was
// measured rather than assumed:
//
//   - A *dead* Ollama is cheap. Connection refused returns in ~3ms, so
//     6,064 rows churn through in about twenty seconds and report 6,064
//     failures — noisy, but survivable.
//   - A *hung* one (overloaded, swapping, a network partition to a remote
//     server) accepts the connection and never answers, so each row costs
//     the full 30s HTTP timeout twice over, once per Embed retry. Timed
//     against a real socket that accepts and never replies: 60.02s. At
//     that rate the 6,064 rows this project's own dev store needed would
//     take **101 hours** — four days, with no output until the very end.
//
// One honest qualification, since it changes when this actually matters:
// an Ollama that is ALREADY hung when the command starts is caught by
// cmdReembed's own dimension probe, which fails after the same measured
// 60s and returns before this loop ever runs. The 101-hour case therefore
// requires Ollama to degrade *after* the probe succeeded — it starts
// swapping under the load of the run itself, gets restarted mid-run, or
// (newly possible now that BaseURLEnvVar allows a remote server) becomes
// unreachable across the network partway through. That is a real
// scenario, not a hypothetical one, but it is narrower than "Ollama is
// down," and the breaker should be understood as covering the mid-run
// case specifically.
//
// Five is deliberately above the noise floor and far below the damage
// threshold. Individual rows do fail in isolation for reasons that say
// nothing about the service (a single oversized text, one transient
// blip), and stopping the whole run for one of those would be worse than
// the disease; five in a row with zero successes between them is not a
// data problem.
const maxConsecutiveFailures = 5

// progressInterval bounds how long a run can go without saying anything.
// Time-based, not row-based, precisely because the pathological case is
// slow rows: "every 100 rows" prints nothing at all for hours when each
// row takes a minute, which is exactly the situation that most needs
// output.
// A var, not a const, solely so tests can shrink it — the pathological
// case this exists for takes minutes per row to reproduce honestly.
var progressInterval = 5 * time.Second

func printReembedProgress(reembedded, failed int, started time.Time) {
	elapsed := time.Since(started)
	rate := float64(reembedded) / elapsed.Seconds()
	fmt.Printf("  … %d re-embedded", reembedded)
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Printf(" — %.0fs elapsed, %.1f rows/s\n", elapsed.Seconds(), rate)
}

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
	dbPath := cli.DBFlag(fs)
	embedModel := fs.String("embed-model", cli.DefaultEmbedModel, "Ollama model to re-embed with")
	project := fs.String("project", "", "scope to one project (default: every project in the store)")
	yes := fs.Bool("yes", false, "actually re-embed — without this, reembed only reports how many rows WOULD be re-embedded")
	fs.Parse(args)

	ctx, cancel := cliContext()
	defer cancel()
	client := embed.NewClient(*embedModel)
	// The current model's real dimension count, learned from the model
	// itself rather than hardcoded — nothing in this codebase maintains a
	// model-name-to-dimension lookup table, and a probe embed call is the
	// only way to know for certain what THIS model actually produces
	// right now.
	probe, err := client.Embed(ctx, "dimension probe")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to reach Ollama with model %q: %v\n", *embedModel, err)
		return 1
	}
	expectedDims := int64(len(probe))

	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	const pageSize = 100
	var afterID int64
	var candidates, reembedded, failed int
	var consecutiveFailures int
	var tripped bool
	started := time.Now()
	lastProgress := started

pager:
	for {
		batch, err := st.ObservationsNeedingEmbedding(ctx, *project, expectedDims, afterID, pageSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED querying observations needing embedding: %v\n", err)
			return 1
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			candidates++
			// Advanced before the dry-run continue, not after the embed:
			// pagination is keyed on afterID, so a dry run that skipped
			// this would re-fetch page 1 forever.
			afterID = r.ID
			if !*yes {
				continue
			}
			text := embed.ObservationText(r.Observation.Title, r.Observation.Subtitle, r.Observation.Narrative, r.Observation.Facts)
			vec, err := client.Embed(ctx, text)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAILED embedding observation id=%d: %v\n", r.ID, err)
				failed++
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					tripped = true
					break pager
				}
				continue
			}
			if err := st.SaveEmbedding(ctx, r.ID, vec); err != nil {
				fmt.Fprintf(os.Stderr, "FAILED saving embedding for observation id=%d: %v\n", r.ID, err)
				failed++
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					tripped = true
					break pager
				}
				continue
			}
			reembedded++
			// A row that succeeds proves the service is alive, so the run
			// that follows deserves a clean slate: the breaker is for a
			// sustained outage, not a cumulative tally that would
			// eventually trip on a long, mostly-healthy run.
			consecutiveFailures = 0
			if time.Since(lastProgress) >= progressInterval {
				printReembedProgress(reembedded, failed, started)
				lastProgress = time.Now()
			}
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
	if tripped {
		// Said separately and on stderr, because "12 failed" and "stopped
		// because the service is down" call for completely different
		// responses, and the old output could not tell them apart. The
		// distinction that matters to the operator is that the remaining
		// rows were never attempted — they are still waiting, not broken —
		// so re-running after fixing Ollama picks up where this left off
		// rather than redoing work.
		fmt.Fprintf(os.Stderr,
			"\nSTOPPED after %d consecutive failures — this looks like %s being down, not bad data.\n"+
				"Nothing was lost: the rows that were never attempted still need embedding, so fix the\n"+
				"embedding service and re-run the same command to continue from here.\n",
			maxConsecutiveFailures, client.BaseURL)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}
