// Package backend dispatches a single connection-string-shaped flag value
// to either the SQLite or Postgres store.Backend implementation — kept
// separate from both so neither store nor postgres has to import the
// other (store is postgres's dependency, not the reverse; a dispatcher
// living in store would create an import cycle).
package backend

import (
	"context"
	"strings"

	"claude-mem-go/postgres"
	"claude-mem-go/store"
)

// Open returns the Postgres backend for a postgres:// or postgresql://
// dsn (real HNSW ANN + full-text search — needs Docker or a real instance
// reachable at that address), or the SQLite backend otherwise, treating
// dsn as a file path. This is claude-mem-go's zero-dependency default
// (SQLite) versus its scale-up path (Postgres), selected by one flag value
// instead of a separate backend-kind flag.
func Open(ctx context.Context, dsn string, embedDims int) (store.Backend, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return postgres.Open(ctx, dsn, embedDims)
	}
	return store.Open(dsn)
}
