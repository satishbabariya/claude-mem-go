package main

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pgvector/pgvector-go"
)

func openRaw(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn)
}

func listProjects(db *sql.DB) ([]string, error) {
	rows, err := db.Query(
		`SELECT project FROM observations WHERE embedding IS NOT NULL
		 GROUP BY project ORDER BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// annScopedCapped replays the EXACT query and session settings
// SemanticSearch uses for a scoped search, with one addition: an explicit
// hnsw.max_scan_tuples.
//
// It exists because the headline scoped result is measured on a corpus
// the same size as that setting's default (20,000), so iterative_scan can
// walk the whole table and the perfect recall it produces cannot be
// distinguished from "the corpus was small enough to scan entirely."
// Lowering the cap on a fixed corpus is equivalent to raising the corpus
// above a fixed cap, and costs minutes instead of the hours re-embedding
// a 200,000-row corpus would.
//
// The tradeoff is stated rather than hidden: this issues the query by
// hand instead of calling SemanticSearch, because the Store deliberately
// exposes no way to set this GUC. The default-cap row is measured through
// the real code path; these rows verify the same SQL under a tighter cap.
func annScopedCapped(db *sql.DB, project string, vec pgvector.Vector, k, maxScan int) ([]float64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// hnsw.* GUCs are custom parameters pgvector registers only once
	// something on this connection has touched the vector type — this
	// project has already documented Postgres silently accepting an
	// out-of-range hnsw.ef_search on an otherwise-idle connection for the
	// same reason. Setting them before that point fails outright here, so
	// the SET order below is load-bearing, not incidental.
	if _, err := tx.Exec("SET LOCAL hnsw.iterative_scan = strict_order"); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(fmt.Sprintf("SET LOCAL hnsw.max_scan_tuples = %d", maxScan)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("SET LOCAL enable_seqscan = off"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT embedding <=> $1 FROM observations
		 WHERE embedding IS NOT NULL AND project = $2
		 ORDER BY embedding <=> $1 LIMIT $3`, vec, project, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// exactScoped is the ground truth: the k genuinely nearest distances
// within one project, from a forced sequential scan.
//
// enable_indexscan/bitmapscan off is what makes this exact. Without it
// the planner would happily use the same HNSW index the measurement is
// trying to grade, and the comparison would silently become
// approximate-vs-approximate — reporting perfect recall for an index that
// is missing the same rows on both sides.
func exactScoped(db *sql.DB, project string, vec pgvector.Vector, k int) ([]float64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL enable_indexscan = off"); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("SET LOCAL enable_bitmapscan = off"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT embedding <=> $1 FROM observations
		 WHERE embedding IS NOT NULL AND project = $2
		 ORDER BY embedding <=> $1 LIMIT $3`, vec, project, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scopedPlan reports which access path Postgres actually chooses for a
// scoped search under a given ef_search, as "HNSW" or "exact".
//
// This exists because a recall number is uninterpretable without it. At
// ef_search 100 and above, on a corpus where each project holds a
// thousand rows, the planner stops using the HNSW index at all and takes
// a bitmap scan over idx_observations_project instead — reading every row
// in the project and sorting exactly. Recall is then 100% because the
// query became exact, not because the approximation improved, and
// reporting that as "recall rose to 100%" would be precisely the
// exact-vs-exact illusion this benchmark exists to avoid.
//
// Raising ef_search causes it: pgvector's cost estimate for an HNSW scan
// grows with ef_search, so past a point the exact alternative simply
// costs less. That is good behaviour — correct results, and cheap while
// projects stay small — but it is a different fact from the one a recall
// column alone appears to state.
func scopedPlan(db *sql.DB, project string, vec pgvector.Vector, k, ef int) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL hnsw.iterative_scan = strict_order"); err != nil {
		return "", err
	}
	if ef > 0 {
		if _, err := tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", ef)); err != nil {
			return "", err
		}
	}
	rows, err := tx.Query(
		`EXPLAIN (COSTS OFF) SELECT id FROM observations
		 WHERE embedding IS NOT NULL AND project = $2
		 ORDER BY embedding <=> $1 LIMIT $3`, vec, project, k)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		if strings.Contains(line, "embedding_hnsw") {
			return "HNSW (approximate)", nil
		}
	}
	return "exact (planner skipped HNSW)", rows.Err()
}
