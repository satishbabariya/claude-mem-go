package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
)

// Real, varied engineering prose — the kind of text this store actually
// holds — so the embeddings have genuine structure. Random vectors are
// near-orthogonal and degenerate for HNSW; that is exactly why the
// earlier scale run refused to quote a recall number.
var subjects = []string{"the auth token cache", "the postgres connection pool", "the HNSW index build",
	"the retry backoff loop", "the session summary prompt", "the file-context hook", "the embedding client",
	"the worker daemon socket", "the prune cutoff", "the migration runner", "the FTS query parser",
	"the observation dedupe hash", "the privacy tag stripper", "the transcript reader", "the log rotation writer"}
var verbs = []string{"was rewritten to", "silently failed to", "now correctly", "was measured while it",
	"regressed when it", "was documented as it", "stopped being able to", "was optimized so it"}
var objects = []string{"handle concurrent writers", "release its slot on eviction", "resolve relative paths",
	"survive a daemon restart", "bound its own retry window", "report which store it uses",
	"distinguish a probe from a payload", "canonicalize what it stores", "drain in-flight work on shutdown",
	"pick the index instead of a sequential scan", "fall back without silencing errors", "page past its own limit"}

// sites supplies the per-row detail that makes every generated text
// UNIQUE, and it exists because of a measured defect rather than a
// stylistic preference.
//
// The three lists above span 15*8*12 = 1,440 distinct sentences — a hard
// ceiling no row count can exceed. A 20,000-row corpus therefore held
// 1,440 distinct texts and 1,440 distinct embeddings, every one repeated
// about fourteen times. That is fatal to the measurement rather than
// merely untidy: when the nearest text appears fourteen times, the exact
// top-10 is ten copies of ONE vector at an identical distance, so which
// ten ids come back is arbitrary tie-breaking that the sequential scan
// and the HNSW walk resolve differently. Recall measured as id overlap
// then reports tie-break agreement, not retrieval quality — which is
// exactly how it produced a non-monotonic 84 -> 100 -> 81 curve that no
// real ANN index can produce.
//
// Appending a concrete file:line — the kind of specific detail real
// observations genuinely carry — makes each row unique via the line
// number while leaving the sentence's meaning, and so its semantic
// neighbourhood, intact.
var sites = []string{"worker/sessions.go", "store/filepath.go", "postgres/search.go",
	"hooks/pretooluse.go", "embed/client.go", "cmd/claude-mem-go/doctor.go"}

func main() {
	ctx := context.Background()
	dsn, n := os.Args[1], 0
	fmt.Sscanf(os.Args[2], "%d", &n)
	st, err := backend.Open(context.Background(), dsn, 0, 0)
	if err != nil {
		panic(err)
	}
	defer st.Close()
	cl := embed.NewClient("nomic-embed-text")
	rng := rand.New(rand.NewSource(42)) // fixed seed: the corpus is reproducible

	start := time.Now()
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("%s %s %s (%s:%d)", subjects[rng.Intn(len(subjects))],
			verbs[rng.Intn(len(verbs))], objects[rng.Intn(len(objects))],
			sites[rng.Intn(len(sites))], i+1)
		title := strings.ToUpper(text[:1]) + text[1:]
		res, err := st.Insert(ctx, "s-recall", "recall-proj", "Bash",
			memory.ContentHash("s-recall", "Bash", title, fmt.Sprint(i)),
			memory.Observation{Type: "discovery", Title: title, Narrative: text}, 0)
		if err != nil {
			panic(err)
		}
		vec, err := cl.Embed(embed.ObservationText(title, "", text, nil))
		if err != nil {
			panic(err)
		}
		if err := st.SaveEmbedding(ctx, res.ID, vec); err != nil {
			panic(err)
		}
		if (i+1)%500 == 0 {
			fmt.Printf("  %d/%d embedded (%.0f/s)\n", i+1, n, float64(i+1)/time.Since(start).Seconds())
		}
	}
	fmt.Printf("  seeded %d real embeddings in %v\n", n, time.Since(start).Round(time.Second))
}
