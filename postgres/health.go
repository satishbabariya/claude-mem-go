package postgres

import (
	"fmt"
	"strconv"
)

// HealthDetails returns backend-specific operational facts for `doctor` —
// see store.Store.HealthDetails' doc comment for the rationale, shared by
// both backends. For Postgres: the connection pool's real utilization
// (database/sql's own Stats — confirming the bound this backend now sets
// via SetMaxOpenConns actually applies, not just that it was requested),
// the pgvector extension's installed version, and whether the HNSW index
// this backend's whole "real ANN search" claim depends on actually
// exists — a schema drift (a manually-run migration, an index dropped by
// hand) would otherwise silently degrade every SemanticSearch call to a
// full table scan without anything here ever saying so.
func (s *Store) HealthDetails() (map[string]string, error) {
	details := map[string]string{}

	dbStats := s.db.Stats()
	details["pool_open_connections"] = strconv.Itoa(dbStats.OpenConnections)
	details["pool_in_use"] = strconv.Itoa(dbStats.InUse)
	details["pool_idle"] = strconv.Itoa(dbStats.Idle)
	details["pool_max_open_connections"] = strconv.Itoa(dbStats.MaxOpenConnections)

	var vectorVersion string
	if err := s.db.QueryRow(`SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&vectorVersion); err != nil {
		details["vector_extension"] = "not installed"
	} else {
		details["vector_extension"] = vectorVersion
	}

	var hnswExists bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_observations_embedding_hnsw')`).Scan(&hnswExists); err != nil {
		return nil, fmt.Errorf("check HNSW index: %w", err)
	}
	details["hnsw_index_exists"] = strconv.FormatBool(hnswExists)

	return details, nil
}
