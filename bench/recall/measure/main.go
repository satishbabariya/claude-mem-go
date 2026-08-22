package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pgvector/pgvector-go"
	"github.com/satishbabariya/claude-mem-go/internal/embed"
)

// Queries phrased differently from how the corpus is worded, so this
// measures semantic retrieval rather than string overlap.
var queries = []string{
	"why did the connection pool stop handing out slots",
	"something about tokens being cached",
	"the index was not being used by the planner",
	"work was lost when the process shut down",
	"paths stored in the wrong form",
	"how retries are bounded",
	"which database the background process writes to",
	"summarising what happened in a session",
	"telling a health probe apart from real data",
	"reading files that were touched before",
}

var efSearch int

// hit pairs a row id with its distance. Recall is computed from the
// DISTANCE, not the id — see recallAt.
type hit struct {
	id   int64
	dist float64
}

func topK(db *sql.DB, vec pgvector.Vector, k int, forceIndex bool) ([]hit, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// enable_seqscan off => the HNSW index is the only path (approximate).
	// on, with indexscan off => a full exact scan (ground truth).
	if forceIndex {
		_, err = tx.Exec("SET LOCAL enable_seqscan = off")
		if err == nil && efSearch > 0 {
			_, err = tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch))
		}
	} else {
		_, err = tx.Exec("SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off")
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT id, embedding <=> $1 AS dist FROM observations
		 WHERE embedding IS NOT NULL ORDER BY embedding <=> $1 LIMIT $2`, vec, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.dist); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// distEpsilon absorbs float noise between the two plans. The distance is
// computed by the same operator either way, but the scan order differs,
// and float addition is not associative.
const distEpsilon = 1e-9

// recallAt is the tie-tolerant definition: how many of the ANN results
// are at least as close as the exact k-th result. Counting id overlap
// instead — the obvious implementation, and this harness's original one —
// silently measures TIE-BREAK AGREEMENT whenever the corpus contains
// duplicate vectors, because the two plans enumerate equidistant rows in
// different orders. On a corpus that was 14x duplicated that produced a
// non-monotonic curve (84 -> 100 -> 81 -> 83 -> 85); recall cannot fall
// as ef_search rises, so the metric was measuring the wrong thing.
//
// Both defences are kept deliberately: the seeder now generates unique
// texts, AND this metric is immune to ties even if some future corpus
// contains them anyway.
func recallAt(exact, ann []hit) (hits, total int) {
	if len(exact) == 0 {
		return 0, 0
	}
	cutoff := exact[len(exact)-1].dist + distEpsilon
	for _, a := range ann {
		if a.dist <= cutoff {
			hits++
		}
	}
	return hits, len(exact)
}

// assertCorpusUsable refuses to report a recall figure the corpus cannot
// support. A vector benchmark whose corpus is mostly duplicates yields a
// number that looks authoritative and means nothing, which is precisely
// the failure this harness already shipped once.
func assertCorpusUsable(db *sql.DB) error {
	var rows, distinct int64
	err := db.QueryRow(
		`SELECT count(*), count(DISTINCT embedding::text) FROM observations WHERE embedding IS NOT NULL`,
	).Scan(&rows, &distinct)
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("corpus is empty — run the seeder first")
	}
	fmt.Printf("  corpus: %d embedded rows, %d distinct vectors\n", rows, distinct)
	// Duplicates are not merely noise here; they make the exact top-k
	// ambiguous. Anything below near-total uniqueness means the generator
	// saturated its combinatorial space.
	if float64(distinct) < 0.99*float64(rows) {
		return fmt.Errorf(
			"corpus is degenerate: only %d distinct vectors across %d rows (%.1f%%).\n"+
				"The exact top-k is then a tie among identical vectors and recall is not measurable.\n"+
				"Re-seed with a generator that produces unique texts",
			distinct, rows, 100*float64(distinct)/float64(rows))
	}
	return nil
}

func main() {
	db, err := sql.Open("pgx", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()

	if err := assertCorpusUsable(db); err != nil {
		fmt.Fprintf(os.Stderr, "\nREFUSING TO MEASURE: %v\n", err)
		os.Exit(1)
	}
	cl := embed.NewClient("nomic-embed-text")

	const k = 10

	// Embed every query ONCE. Re-embedding inside the ef loop would fold
	// Ollama's latency into the per-ef timing below and swamp the
	// milliseconds actually being compared.
	vecs := make([]pgvector.Vector, len(queries))
	for i, q := range queries {
		v, err := cl.Embed(q)
		if err != nil {
			panic(err)
		}
		vecs[i] = pgvector.NewVector(v)
	}

	fmt.Printf("\n  %-12s %-11s %-11s %s\n", "ef_search", "recall@10", "p50 query", "note")
	var prev float64 = -1
	var nonMonotonic bool
	for _, ef := range []int{20, 40, 100, 200, 400} {
		efSearch = ef
		total, hits := 0, 0
		var lat []time.Duration
		for _, vec := range vecs {
			exact, err := topK(db, vec, k, false)
			if err != nil {
				panic(err)
			}
			// Timed on a warm index: the first ANN call per ef primes the
			// cache, and reporting the cold call as if it were typical
			// would overstate the cost of raising this knob.
			if _, err := topK(db, vec, k, true); err != nil {
				panic(err)
			}
			started := time.Now()
			ann, err := topK(db, vec, k, true)
			if err != nil {
				panic(err)
			}
			lat = append(lat, time.Since(started))
			h, t := recallAt(exact, ann)
			hits += h
			total += t
		}
		r := 100 * float64(hits) / float64(total)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p50 := lat[len(lat)/2]
		note := ""
		if ef == 40 {
			note = "<- pgvector default"
		}
		// Recall is monotonically non-decreasing in ef_search by
		// construction: a larger candidate list explores strictly more of
		// the graph. A drop is a defect in this harness, not a property of
		// the index, and saying so beats quietly printing the number.
		if prev >= 0 && r < prev-0.05 {
			note += "  !! DROPPED vs previous ef — impossible for HNSW; harness bug"
			nonMonotonic = true
		}
		prev = r
		fmt.Printf("  %-12d %-11.1f %-11s %s\n", ef, r, p50.Round(100*time.Microsecond), note)
	}
	if nonMonotonic {
		fmt.Fprintln(os.Stderr, "\nFAILED: recall fell as ef_search rose. Do not quote these numbers.")
		os.Exit(1)
	}
}
