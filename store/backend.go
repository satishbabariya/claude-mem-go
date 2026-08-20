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
	Close() error
}

// Compile-time check that *Store actually satisfies Backend — catches a
// signature drift between this file and store.go/search.go/vector.go at
// build time instead of at the first call site that tries to pass a *Store
// where a Backend is expected.
var _ Backend = (*Store)(nil)
