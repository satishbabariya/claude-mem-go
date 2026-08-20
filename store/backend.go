// Backend is the storage contract every caller (cmd, worker, mcpserver)
// depends on, instead of the concrete SQLite *Store — so a production
// deployment can swap in the postgres package (real HNSW ANN vector search
// + real full-text search, via Docker) without those callers changing at
// all. *Store satisfies this interface already; see the postgres package
// for the second implementation.
//
// This exists because of a direct, explicit ask: "in future look at prod
// database and vector search... i have docker running so you can use
// that." SQLite/brute-force-cosine remains the zero-dependency default;
// Postgres+pgvector is the scale-up path when you actually need it.
package store

type Backend interface {
	Insert(sessionID, project, toolName, contentHash string, o Observation, costUSD float64) (InsertResult, error)
	CountByProject(project string) (int, error)
	// Search full-text-searches observations; project scopes it to one
	// project when non-empty, or every project in the store when empty. See
	// Store.Search's doc comment for why an empty project is a deliberate,
	// narrow exception rather than the default.
	Search(project, query string, limit int) ([]SearchResult, error)
	SaveEmbedding(observationID int64, vec []float32) error
	// SemanticSearch is Search's embedding-based counterpart; project has
	// the same scoping meaning.
	SemanticSearch(project string, queryVec []float32, limit int) ([]VectorMatch, error)
	// RecentByProject returns a project's most recent observations, newest
	// first — the read path for SessionStart context injection (see the
	// context subcommand): the actual "memory" half of claude-mem, as
	// opposed to the on-demand Search/SemanticSearch tools.
	RecentByProject(project string, limit int) ([]SearchResult, error)
	// BySessionID returns every observation recorded for one Claude Code
	// session, oldest first — the read path for Stop-hook session
	// summarization: what actually happened this session, in order.
	BySessionID(sessionID string, limit int) ([]SearchResult, error)
	// ObservationsForFile returns observations whose files_read or
	// files_modified mentions filePath — the read path for PreToolUse's
	// file-context hook.
	ObservationsForFile(project, filePath string, limit int) ([]SearchResult, error)
	// ByIDs fetches specific observations by ID — the read path for a
	// caller that already has IDs (from a prior Search/RecentByProject
	// call) and wants full details the abbreviated list formats omit.
	// Unknown IDs are silently omitted rather than erroring.
	ByIDs(ids []int64) ([]SearchResult, error)
	// Prune deletes observations older than cutoffEpoch (a Unix seconds
	// timestamp), scoped to one project when non-empty or every project
	// when empty. dryRun counts what WOULD be deleted without deleting
	// anything. This is the store's retention story — without it, the
	// store only ever grows.
	Prune(project string, cutoffEpoch int64, dryRun bool) (int64, error)
	// ExportAll returns up to limit observations with id > afterID, oldest
	// first — call repeatedly with the previous page's last ID until a
	// page comes back with fewer than limit rows. This backend's only
	// backup/migration story: paginated so exporting a large store doesn't
	// require loading it all into memory.
	ExportAll(afterID int64, limit int) ([]ExportRow, error)
	// ImportRow re-inserts a previously exported row, preserving its
	// original ContentHash (idempotent-dedup, same as Insert) and its
	// original CreatedAt/CreatedAtEpoch — a restore reflects when things
	// actually happened, not when they were re-imported. Also what makes
	// export+import double as the SQLite<->Postgres migration path.
	ImportRow(row ExportRow) (InsertResult, error)
	// HealthDetails returns backend-specific operational facts `doctor`
	// prints — details generic to this interface can't surface, because
	// they're about how each backend actually runs (SQLite's PRAGMA
	// settings; Postgres's connection pool utilization, pgvector
	// extension version, and whether its HNSW index still exists), not
	// what it stores.
	HealthDetails() (map[string]string, error)
	Close() error
}

// Compile-time check that *Store actually satisfies Backend — catches a
// signature drift between this file and store.go/search.go/vector.go at
// build time instead of at the first call site that tries to pass a *Store
// where a Backend is expected.
var _ Backend = (*Store)(nil)
