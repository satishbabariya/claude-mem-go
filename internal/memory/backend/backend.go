// Package backend dispatches a single connection-string-shaped flag value
// to either the SQLite or Postgres memory.Backend implementation. It is
// its own package rather than a function in memory because it has to
// import both implementations, and they both import memory — a dispatcher
// inside memory would be an import cycle.
package backend

import (
	"context"
	"strings"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/postgres"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"
)

// Open returns the Postgres backend for a postgres:// or postgresql://
// dsn (real HNSW ANN + full-text search — needs Docker or a real instance
// reachable at that address), or the SQLite backend otherwise, treating
// dsn as a file path. This is claude-mem-go's zero-dependency default
// (SQLite) versus its scale-up path (Postgres), selected by one flag value
// instead of a separate backend-kind flag.
//
// hnswEfSearch is ignored entirely for a SQLite dsn — it only ever
// affects postgres.Open's pgvector tuning — so every caller that doesn't
// care can pass 0, the same "0 means use the default" convention embedDims
// already established.
func Open(ctx context.Context, dsn string, embedDims, hnswEfSearch int) (memory.Backend, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return postgres.Open(ctx, dsn, embedDims, hnswEfSearch)
	}
	return sqlite.Open(ctx, dsn)
}
