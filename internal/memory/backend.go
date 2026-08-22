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
package memory

import "context"

type Backend interface {
	Insert(ctx context.Context, sessionID, project, toolName, contentHash string, o Observation, costUSD float64) (InsertResult, error)
	CountByProject(ctx context.Context, project string) (int, error)
	// Search full-text-searches observations; project scopes it to one
	// project when non-empty, or every project in the store when empty. See
	// Store.Search's doc comment for why an empty project is a deliberate,
	// narrow exception rather than the default. obsType additionally
	// filters to one observation type (discovery/change/decision/summary/
	// manual) when non-empty. offset skips that many leading results
	// (after ranking, before limit) — the pagination real claude-mem's own
	// search tool has and this one lacked until now; 0 behaves exactly as
	// it always has. dateStartMs/dateEndMs (Unix epoch milliseconds, 0 =
	// unbounded on that side) filter to created_at_epoch >= dateStartMs
	// and/or <= dateEndMs, real claude-mem's own dateStart/dateEnd search
	// filters. orderBy selects "relevance" (the default when empty — rank
	// order) or "date_desc"/"date_asc"; any other value is treated as
	// date_desc, matching real claude-mem's own SessionSearch.buildOrderClause
	// fallback.
	Search(ctx context.Context, project, query, obsType string, limit, offset int, dateStartMs, dateEndMs int64, orderBy string) ([]SearchResult, error)
	SaveEmbedding(ctx context.Context, observationID int64, vec []float32) error
	// SemanticSearch is Search's embedding-based counterpart; project has
	// the same scoping meaning.
	SemanticSearch(ctx context.Context, project string, queryVec []float32, limit int) ([]VectorMatch, error)
	// RecentByProject returns a project's most recent observations, newest
	// first — the read path for SessionStart context injection (see the
	// context subcommand): the actual "memory" half of claude-mem, as
	// opposed to the on-demand Search/SemanticSearch tools.
	RecentByProject(ctx context.Context, project string, limit int) ([]SearchResult, error)
	// BySessionID returns every observation recorded for one Claude Code
	// session, oldest first — the read path for Stop-hook session
	// summarization: what actually happened this session, in order.
	// project has the same scoping meaning as Search's: non-empty
	// restricts to that project, empty means every project.
	BySessionID(ctx context.Context, project, sessionID string, limit int) ([]SearchResult, error)
	// ObservationsForFile returns observations whose files_read or
	// files_modified mentions filePath — the read path for PreToolUse's
	// file-context hook.
	ObservationsForFile(ctx context.Context, project, filePath string, limit int) ([]SearchResult, error)
	// ByIDs fetches specific observations by ID — the read path for a
	// caller that already has IDs (from a prior Search/RecentByProject
	// call) and wants full details the abbreviated list formats omit.
	// Unknown IDs are silently omitted rather than erroring.
	ByIDs(ctx context.Context, ids []int64) ([]SearchResult, error)
	// Timeline returns up to depthBefore observations immediately before
	// anchorID and up to depthAfter immediately after it, in chronological
	// order with the anchor itself included — "what happened around this
	// specific observation," the read path for the `timeline` MCP tool.
	// Always scoped to the anchor's own project; project is a caller
	// assertion checked against that, not an independent filter.
	Timeline(ctx context.Context, project string, anchorID int64, depthBefore, depthAfter int) ([]SearchResult, error)
	// ObservationsNeedingEmbedding returns observations with no embedding
	// at all, or whose stored embedding dimension doesn't match
	// expectedDims — the read path for the `reembed` CLI command, the
	// remediation half of HealthDetails' embedding_dims_consistent
	// finding. Paginated like ExportAll (id > afterID, oldest first).
	ObservationsNeedingEmbedding(ctx context.Context, project string, expectedDims int64, afterID int64, limit int) ([]SearchResult, error)
	// Prune deletes observations older than cutoffEpoch (a Unix seconds
	// timestamp), scoped to one project when non-empty or every project
	// when empty. dryRun counts what WOULD be deleted without deleting
	// anything. This is the store's retention story — without it, the
	// store only ever grows.
	Prune(ctx context.Context, project string, cutoffEpoch int64, dryRun bool) (int64, error)
	// ExportAll returns up to limit observations with id > afterID, oldest
	// first — call repeatedly with the previous page's last ID until a
	// page comes back with fewer than limit rows. This backend's only
	// backup/migration story: paginated so exporting a large store doesn't
	// require loading it all into
	ExportAll(ctx context.Context, afterID int64, limit int) ([]ExportRow, error)
	// ImportRow re-inserts a previously exported row, preserving its
	// original ContentHash (idempotent-dedup, same as Insert) and its
	// original CreatedAt/CreatedAtEpoch — a restore reflects when things
	// actually happened, not when they were re-imported. Also what makes
	// export+import double as the SQLite<->Postgres migration path.
	ImportRow(ctx context.Context, row ExportRow) (InsertResult, error)
	// HealthDetails returns backend-specific operational facts `doctor`
	// prints — details generic to this interface can't surface, because
	// they're about how each backend actually runs (SQLite's PRAGMA
	// settings; Postgres's connection pool utilization, pgvector
	// extension version, and whether its HNSW index still exists), not
	// what it stores.
	HealthDetails(ctx context.Context) (map[string]string, error)
	// Stats returns what the store actually CONTAINS, as opposed to
	// HealthDetails' "is the machinery working".
	//
	// Those are different questions and only the second was answerable
	// before this. Every check `doctor` ran was a reachability check, so
	// it could report "All critical checks passed" while capture had been
	// silently dead for weeks — which is this architecture's most likely
	// failure, not its least: PostToolUse is fire-and-forget, so a hook
	// that fails writes to a log nobody reads; a plugin binary can go
	// stale; one typo in an excluded-projects glob silently excludes
	// everything. In all of those cases every component is reachable and
	// the store simply stops growing, which nothing measured.
	Stats(ctx context.Context) (StoreStats, error)
	Close() error
}

// StoreStats is a factual description of a store's contents — the local
// equivalent of the db_observation_count / db_session_count /
// db_project_count / days_since_last_obs figures real claude-mem reports
// through outbound telemetry. Reported to the operator here rather than
// sent anywhere, which is the same stance this port takes on cloud sync.
type StoreStats struct {
	Observations int
	Projects     int
	Sessions     int
	// ByType counts observations per type (discovery/change/decision/
	// summary/manual) — a store with no `summary` rows means the Stop
	// hook is not firing, which nothing else surfaces.
	ByType map[string]int
	// Embedded is how many observations have an embedding at all. The
	// gap between this and Observations is what semantic search cannot
	// see.
	Embedded int
	// OldestEpochMs and NewestEpochMs are 0 for an empty store. Newest is
	// the one that answers "is capture still happening".
	OldestEpochMs int64
	NewestEpochMs int64
}
