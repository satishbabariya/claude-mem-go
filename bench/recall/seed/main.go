package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/store"
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

func main() {
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
		text := fmt.Sprintf("%s %s %s", subjects[rng.Intn(len(subjects))],
			verbs[rng.Intn(len(verbs))], objects[rng.Intn(len(objects))])
		title := strings.ToUpper(text[:1]) + text[1:]
		res, err := st.Insert("s-recall", "recall-proj", "Bash",
			store.ContentHash("s-recall", "Bash", title, fmt.Sprint(i)),
			store.Observation{Type: "discovery", Title: title, Narrative: text}, 0)
		if err != nil {
			panic(err)
		}
		vec, err := cl.Embed(embed.ObservationText(title, "", text, nil))
		if err != nil {
			panic(err)
		}
		if err := st.SaveEmbedding(res.ID, vec); err != nil {
			panic(err)
		}
		if (i+1)%500 == 0 {
			fmt.Printf("  %d/%d embedded (%.0f/s)\n", i+1, n, float64(i+1)/time.Since(start).Seconds())
		}
	}
	fmt.Printf("  seeded %d real embeddings in %v\n", n, time.Since(start).Round(time.Second))
}
