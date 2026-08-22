package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// HealthDetails returns backend-specific operational facts for `doctor` —
// see sqlite.Store.HealthDetails' doc comment for the rationale, shared by
// both backends. For Postgres: the connection pool's real utilization
// (database/sql's own Stats — confirming the bound this backend now sets
// via SetMaxOpenConns actually applies, not just that it was requested),
// the pgvector extension's installed version, and whether the HNSW index
// this backend's whole "real ANN search" claim depends on actually
// exists — a schema drift (a manually-run migration, an index dropped by
// hand) would otherwise silently degrade every SemanticSearch call to a
// full table scan without anything here ever saying so.
func (s *Store) HealthDetails(ctx context.Context) (map[string]string, error) {
	details := map[string]string{}

	dbStats := s.db.Stats()
	details["pool_open_connections"] = strconv.Itoa(dbStats.OpenConnections)
	details["pool_in_use"] = strconv.Itoa(dbStats.InUse)
	details["pool_idle"] = strconv.Itoa(dbStats.Idle)
	details["pool_max_open_connections"] = strconv.Itoa(dbStats.MaxOpenConnections)

	var vectorVersion string
	if err := s.db.QueryRowContext(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&vectorVersion); err != nil {
		details["vector_extension"] = "not installed"
	} else {
		details["vector_extension"] = vectorVersion
	}

	var hnswExists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_observations_embedding_hnsw')`).Scan(&hnswExists); err != nil {
		return nil, fmt.Errorf("check HNSW index: %w", err)
	}
	details["hnsw_index_exists"] = strconv.FormatBool(hnswExists)

	// hnsw_ef_search reports this Store's *configured* override (see Open's
	// hnswEfSearch parameter), not a live Postgres session value — there
	// isn't one to report: SemanticSearch applies it via a transaction-
	// scoped SET LOCAL for the duration of one query, not a persistent
	// session setting on any of this pool's connections (see
	// SemanticSearch's own doc comment for why). Still worth surfacing
	// here: this is the one tuning knob `doctor` had no visibility into at
	// all before this — an operator who set -hnsw-ef-search had no way to
	// confirm it was actually configured short of reading the process's
	// own flags.
	if s.hnswEfSearch > 0 {
		details["hnsw_ef_search"] = strconv.Itoa(s.hnswEfSearch)
	} else {
		details["hnsw_ef_search"] = "default (40)"
	}

	// embedding_dims: reported for parity with the SQLite backend's
	// identical key, but this backend can never actually show more than
	// one distinct dimension — the embedding column's type is a fixed
	// vector(N) set once at schema creation (see Open's embedDims
	// parameter), so a dimension mismatch fails loudly at SaveEmbedding
	// time instead of silently degrading SemanticSearch the way it can on
	// SQLite (confirmed directly: saving a wrong-dimension vector here
	// returns a real Postgres error, "expected N dimensions, not M").
	// vector_dims() is pgvector's own accessor.
	// The embedding COLUMN's width, distinct from the histogram of
	// dimensions actually stored below. Fixed when the store is created
	// and unchangeable afterwards, so it's the number that decides
	// whether a given embedding model can write here at all — the single
	// most useful fact when semantic search stops working after someone
	// switches models. An empty histogram plus a known column width is a
	// perfectly diagnosable state; the column width alone was previously
	// invisible.
	if s.embedDims > 0 {
		details["embedding_column_dims"] = strconv.Itoa(s.embedDims)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT vector_dims(embedding), COUNT(*) FROM observations WHERE embedding IS NOT NULL GROUP BY vector_dims(embedding) ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("query embedding dims histogram: %w", err)
	}
	var histogram []string
	for rows.Next() {
		var dims, count int
		if err := rows.Scan(&dims, &count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan embedding dims histogram: %w", err)
		}
		histogram = append(histogram, fmt.Sprintf("%d:%d", dims, count))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query embedding dims histogram: %w", err)
	}
	rows.Close()
	if len(histogram) > 0 {
		details["embedding_dims"] = strings.Join(histogram, ",")
		details["embedding_dims_consistent"] = strconv.FormatBool(len(histogram) == 1)
	}

	return details, nil
}
