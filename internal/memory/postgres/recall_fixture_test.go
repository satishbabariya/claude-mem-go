package postgres

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"github.com/pgvector/pgvector-go"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// loadRecallFixture reads testdata/recall_fixture.f32.gz: 300 real
// nomic-embed-text vectors taken from the bench/recall corpus, stored as a
// little-endian (count, dims) header followed by float32s. Real embeddings
// matter: random vectors are near-orthogonal and make HNSW look perfect.
func loadRecallFixture(t *testing.T) [][]float32 {
	t.Helper()
	f, err := os.Open("testdata/recall_fixture.f32.gz")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip fixture: %v", err)
	}
	var hdr [2]uint32
	if err := binary.Read(gz, binary.LittleEndian, &hdr); err != nil {
		t.Fatalf("fixture header: %v", err)
	}
	n, dims := int(hdr[0]), int(hdr[1])
	if dims != DefaultEmbedDims {
		t.Fatalf("fixture dims %d != DefaultEmbedDims %d", dims, DefaultEmbedDims)
	}
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dims)
		if err := binary.Read(gz, binary.LittleEndian, v); err != nil {
			t.Fatalf("fixture vector %d: %v", i, err)
		}
		out[i] = v
	}
	return out
}

type hit struct {
	id   int64
	dist float64
}

// topK mirrors bench/recall/measure: the exact path forbids the index, the
// ANN path forbids the sequential scan, so the planner's own choice — which
// on a small table is always seqscan — cannot turn this into exact-vs-exact.
func topK(t *testing.T, db *sql.DB, project string, vec []float32, k, efSearch int) []hit {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if efSearch > 0 {
		if _, err := tx.Exec("SET LOCAL enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
			t.Fatal(err)
		}
	} else if _, err := tx.Exec("SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off"); err != nil {
		t.Fatal(err)
	}
	scope, args := "", []any{pgvector.NewVector(vec), k}
	if project != "" {
		scope, args = " AND project = $3", append(args, project)
	}
	rows, err := tx.Query(`SELECT id, embedding <=> $1 AS dist FROM observations
		WHERE embedding IS NOT NULL`+scope+` ORDER BY embedding <=> $1 LIMIT $2`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.dist); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// TestSemanticSearchDefaultEfSearchIsInEffect is the CI-sized guard for
// issue #6. A 300-row HNSW graph is too small to show approximation loss
// (every ef_search recalls 100%), so instead of re-measuring the bench
// figures this pins what a CI run CAN prove on real embeddings:
//
//   - SemanticSearch on a default Store returns a full top-10 with >= 90%
//     of the exact top-10 (the post-filter bug stays fixed; the default is
//     not silently worse than exact).
//   - A Store opened with hnsw.ef_search = 3 returns at most 3 rows for
//     LIMIT 10: pgvector bounds a plain index scan's results by ef_search,
//     so this proves the per-transaction SET LOCAL reaches the query. If
//     someone reverted SemanticSearch to pgvector's session default, the
//     low store would return 10 rows and this fails.
//
// Unscoped on purpose: the scoped path uses hnsw.iterative_scan, which
// keeps scanning until LIMIT is met and so hides the ef_search bound.
func TestSemanticSearchDefaultEfSearchIsInEffect(t *testing.T) {
	// Both stores force the index via a connection-level GUC. On a fresh,
	// small CI database the planner prefers a sequential scan for the
	// unscoped query, which is exact and ignores ef_search entirely — the
	// same trap bench/recall's README describes — so without this the
	// ef_search=3 bound below is unobservable and the guard proves nothing.
	dsn := requireTestDSN(t) + "&options=-c%20enable_seqscan%3Doff"
	ctx := context.Background()
	st, err := Open(ctx, dsn, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vecs := loadRecallFixture(t)
	proj := uniqueProject(t)
	t.Cleanup(func() { st.Prune(ctx, proj, 1<<62, false) })
	for i, v := range vecs {
		res, err := st.Insert(ctx, "recall", proj, "Bash", fmt.Sprintf("%s-%d", proj, i),
			memory.Observation{Type: "discovery", Title: fmt.Sprintf("fixture %d", i)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveEmbedding(ctx, res.ID, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec("ANALYZE observations"); err != nil {
		t.Fatal(err)
	}
	low, err := Open(ctx, dsn, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer low.Close()

	const k, queries = 10, 30
	hits, bounded := 0, 0
	for q := 0; q < queries; q++ {
		query := vecs[q*7%len(vecs)]
		exact := topK(t, st.db, "", query, k, 0)
		if len(exact) != k {
			t.Fatalf("exact top-%d returned %d rows", k, len(exact))
		}
		got, err := st.SemanticSearch(ctx, "", query, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != k {
			t.Fatalf("default store returned %d rows for LIMIT %d — empty/short results are the post-filter bug", len(got), k)
		}
		cutoff := exact[len(exact)-1].dist + 1e-6
		for _, m := range got {
			if 1-m.Score <= cutoff {
				hits++
			}
		}
		lowGot, err := low.SemanticSearch(ctx, "", query, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(lowGot) <= 3 {
			bounded++
		}
	}
	if recall := float64(hits) / float64(queries*k); recall < 0.9 {
		t.Fatalf("default ef_search recall@%d = %.1f%% vs exact; want >= 90%%", k, 100*recall)
	} else {
		t.Logf("default ef_search (%d): recall@%d = %.1f%%", DefaultHNSWEfSearch, k, 100*recall)
	}
	// "At least one", not "every": on a database that has accumulated
	// duplicate vectors from earlier test runs, ties let the scan emit more
	// than ef_search rows for some query vectors (observed: 3 rows for one
	// vector, 10 for another, same settings). A SET LOCAL that never
	// reached the engine returns 10 rows for EVERY query, which is what
	// this guards against.
	if bounded == 0 {
		t.Fatalf("ef_search=3 store returned more than 3 rows on all %d queries — SET LOCAL hnsw.ef_search is not reaching the query", queries)
	}
	t.Logf("ef_search=3 store bounded to <= 3 rows on %d/%d queries", bounded, queries)
}
