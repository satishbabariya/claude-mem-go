// Command scoped measures recall for PROJECT-SCOPED semantic search —
// the case every hook actually uses, and the one the unscoped benchmark
// deliberately does not cover.
//
// Scoping is not a small variation on the unscoped query. A project
// filter is a POST-filter on the HNSW scan, so pgvector walks
// hnsw.ef_search globally-nearest candidates and only then drops the ones
// whose project doesn't match — which this project has already caught
// returning ZERO rows for a project holding 60,000 embedded observations.
// SemanticSearch fixes that with hnsw.iterative_scan, an entirely
// different traversal from the one the unscoped numbers describe, so its
// recall is a separate question that had never been asked.
//
// This deliberately measures through the real postgres.Store.SemanticSearch
// rather than hand-written SQL: the thing worth knowing is what the
// shipping code path returns, including its plan selection, not what an
// idealized query would.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory/postgres"

	"github.com/pgvector/pgvector-go"
)

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

const distEpsilon = 1e-9

func main() {
	dsn := os.Args[1]
	ctx := context.Background()

	st, err := postgres.Open(ctx, dsn, 0, 0)
	if err != nil {
		panic(err)
	}
	defer st.Close()

	// A raw handle alongside the Store, used ONLY to compute ground truth
	// with a forced exact scan. The Store's own API deliberately offers no
	// way to ask for an exact scan, which is correct for production and
	// useless for measuring approximation error.
	raw, err := openRaw(dsn)
	if err != nil {
		panic(err)
	}
	defer raw.Close()

	projects, err := listProjects(raw)
	if err != nil {
		panic(err)
	}
	if len(projects) < 2 {
		fmt.Fprintf(os.Stderr, "REFUSING TO MEASURE: found %d project(s); scoped recall is meaningless "+
			"without several projects competing for the same globally-nearest candidates\n", len(projects))
		os.Exit(1)
	}
	fmt.Printf("  corpus: %d projects\n", len(projects))

	cl := embed.NewClient("nomic-embed-text")
	const k = 10

	vecs := make([]pgvector.Vector, len(queries))
	for i, q := range queries {
		v, err := cl.Embed(q)
		if err != nil {
			panic(err)
		}
		vecs[i] = pgvector.NewVector(v)
	}

	var hits, total, short, empty int
	var lat []time.Duration
	// Every query against every project: scoped recall depends on where
	// the target project's rows happen to fall relative to the global
	// nearest, so sampling one project would measure luck.
	for _, vec := range vecs {
		for _, proj := range projects {
			truth, err := exactScoped(raw, proj, vec, k)
			if err != nil {
				panic(err)
			}
			if len(truth) == 0 {
				continue
			}
			started := time.Now()
			got, err := st.SemanticSearch(proj, vec.Slice(), k)
			if err != nil {
				panic(err)
			}
			lat = append(lat, time.Since(started))

			cutoff := truth[len(truth)-1] + distEpsilon
			for _, m := range got {
				// SemanticSearch reports cosine SIMILARITY; ground truth is
				// cosine DISTANCE. 1-score converts back.
				if 1-m.Score <= cutoff {
					hits++
				}
			}
			total += len(truth)
			// Returning fewer rows than exist is the actual production
			// symptom of the post-filter bug — not an error, just quietly
			// less memory than the store holds.
			if len(got) < len(truth) {
				short++
			}
			if len(got) == 0 {
				empty++
			}
		}
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Printf("\n  scoped recall@%d   %.1f%%  (%d/%d)\n", k, 100*float64(hits)/float64(total), hits, total)
	fmt.Printf("  short results     %d/%d\n", short, len(lat))
	fmt.Printf("  empty results     %d/%d\n", empty, len(lat))
	fmt.Printf("  p50 / p95         %v / %v\n",
		lat[len(lat)/2].Round(100*time.Microsecond),
		lat[len(lat)*95/100].Round(100*time.Microsecond))

	if empty > 0 {
		fmt.Fprintf(os.Stderr, "\nFAILED: %d scoped searches returned NOTHING while the project holds matching rows.\n", empty)
		os.Exit(1)
	}

	// Does the same knob that fixes unscoped recall fix the scoped path
	// too? Measured through the real Store, which takes the override as an
	// Open parameter, so this is the shipping code path rather than a
	// hand-issued approximation of it.
	fmt.Printf("\n  %-14s %-10s %s\n", "ef_search", "recall@10", "plan actually used")
	for _, ef := range []int{0, 100, 200, 400} {
		tuned, err := postgres.Open(ctx, dsn, 0, ef)
		if err != nil {
			panic(err)
		}
		var h, tot int
		for _, vec := range vecs {
			for _, proj := range projects {
				truth, err := exactScoped(raw, proj, vec, k)
				if err != nil {
					panic(err)
				}
				if len(truth) == 0 {
					continue
				}
				got, err := tuned.SemanticSearch(proj, vec.Slice(), k)
				if err != nil {
					panic(err)
				}
				cutoff := truth[len(truth)-1] + distEpsilon
				for _, m := range got {
					if 1-m.Score <= cutoff {
						h++
					}
				}
				tot += len(truth)
			}
		}
		tuned.Close()
		label := fmt.Sprintf("%d", ef)
		if ef == 0 {
			label = "default (40)"
		}
		// Printed beside the recall figure because the number means
		// something completely different depending on which path ran.
		plan, err := scopedPlan(raw, projects[0], vecs[0], k, ef)
		if err != nil {
			panic(err)
		}
		fmt.Printf("  %-14s %-10s %s\n", label, fmt.Sprintf("%.1f%%", 100*float64(h)/float64(tot)), plan)
	}

	// The result above is measured on a corpus the same size as
	// hnsw.max_scan_tuples' default, so iterative_scan can walk the entire
	// table and perfect recall proves less than it appears to. Squeezing
	// the cap simulates the corpus growing past it.
	var rows int
	if err := raw.QueryRow(`SELECT count(*) FROM observations WHERE embedding IS NOT NULL`).Scan(&rows); err != nil {
		panic(err)
	}
	fmt.Printf("\n  Simulating a larger corpus by lowering hnsw.max_scan_tuples (default 20000)\n")
	fmt.Printf("  on these %d rows — a cap of C is equivalent to a corpus of %d*20000/C rows.\n\n", rows, rows)
	fmt.Printf("  %-14s %-16s %s\n", "max_scan", "~equivalent rows", "scoped recall@10")
	for _, cap := range []int{20000, 10000, 4000, 2000, 1000} {
		var h, tot int
		for _, vec := range vecs {
			for _, proj := range projects {
				truth, err := exactScoped(raw, proj, vec, k)
				if err != nil {
					panic(err)
				}
				if len(truth) == 0 {
					continue
				}
				got, err := annScopedCapped(raw, proj, vec, k, cap)
				if err != nil {
					panic(err)
				}
				cutoff := truth[len(truth)-1] + distEpsilon
				for _, d := range got {
					if d <= cutoff {
						h++
					}
				}
				tot += len(truth)
			}
		}
		fmt.Printf("  %-14d %-16d %.1f%%\n", cap, rows*20000/cap, 100*float64(h)/float64(tot))
	}
}
