// Package postgres is claude-mem-go's production-scale storage backend:
// PostgreSQL + pgvector, implementing store.Backend exactly like the
// default SQLite store.Store does — a caller depending on store.Backend
// (worker, cmd, mcpserver) doesn't know or care which one it's talking to.
//
// This exists to close the honest limitation the SQLite backend documents
// up front: brute-force cosine similarity doesn't scale past a few thousand
// rows. Postgres+pgvector gives a real ANN index (HNSW) and real full-text
// search (tsvector/GIN) instead of hand-rolled substitutes — see
// docker-compose.yml for the pgvector/pgvector:pg16 image this targets.
//
// Full-text search here also sidesteps a real bug the SQLite/FTS5 backend
// had to work around by hand (sanitizeFTSQuery, for the "claude-mem" query
// that broke FTS5's grammar on its own hyphen): Postgres's plainto_tsquery
// tokenizes punctuation sanely by default, so no equivalent sanitizer is
// needed here.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pgvector/pgvector-go"

	"claude-mem-go/migrate"
	"claude-mem-go/store"
)

// DefaultEmbedDims matches the embed package's default Ollama model
// (nomic-embed-text, 768 dimensions) — see embed/ollama.go. pgvector
// columns have a fixed dimensionality, so this must match whatever
// embedding model actually produces the vectors SaveEmbedding receives; a
// mismatch fails loudly at insert time rather than silently corrupting data.
const DefaultEmbedDims = 768

// Store is the Postgres-backed store.Backend implementation.
type Store struct {
	db *sql.DB
}

var _ store.Backend = (*Store)(nil)

const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS observations (
	id                BIGSERIAL PRIMARY KEY,
	session_id        TEXT NOT NULL,
	project           TEXT NOT NULL,
	tool_name         TEXT NOT NULL,
	type              TEXT NOT NULL,
	title             TEXT,
	subtitle          TEXT,
	facts             JSONB NOT NULL DEFAULT '[]',
	narrative         TEXT,
	concepts          JSONB NOT NULL DEFAULT '[]',
	files_read        JSONB NOT NULL DEFAULT '[]',
	files_modified    JSONB NOT NULL DEFAULT '[]',
	cost_usd          DOUBLE PRECISION NOT NULL DEFAULT 0,
	created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	created_at_epoch  BIGINT NOT NULL,
	content_hash      TEXT NOT NULL UNIQUE,
	embedding         vector(%d),
	search_vector     tsvector GENERATED ALWAYS AS (
		setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
		setweight(to_tsvector('english', coalesce(subtitle, '')), 'B') ||
		setweight(to_tsvector('english', coalesce(narrative, '')), 'C')
	) STORED
);

CREATE INDEX IF NOT EXISTS idx_observations_project ON observations(project);
CREATE INDEX IF NOT EXISTS idx_observations_session ON observations(session_id);
CREATE INDEX IF NOT EXISTS idx_observations_type ON observations(type);
CREATE INDEX IF NOT EXISTS idx_observations_created ON observations(created_at_epoch DESC);

-- GIN index over the generated tsvector — real full-text search, not a
-- hand-rolled shadow table.
CREATE INDEX IF NOT EXISTS idx_observations_search_vector ON observations USING GIN(search_vector);

-- HNSW: the actual ANN index the SQLite backend's brute-force cosine scan
-- doesn't have. vector_cosine_ops matches the <=> operator used in
-- SemanticSearch below.
CREATE INDEX IF NOT EXISTS idx_observations_embedding_hnsw
	ON observations USING hnsw (embedding vector_cosine_ops);
`

// connectRetryBackoff is Open's retry schedule for the initial ping — every
// real caller in this project (the worker daemon, every CLI hook/subcommand,
// the MCP server) passes context.Background() or a signal-only context with
// no deadline of its own, so without an internal bound a genuinely-down
// Postgres would either hang a hook past Claude Code's own timeout or hang
// an interactive CLI command indefinitely. Sized to smooth a real, common
// startup race — this daemon (or a hook) starting before Postgres's own
// container finishes coming up (docker-compose.yml's healthcheck allows up
// to 40s for that) — without turning a genuinely-down Postgres into a long
// hang: ~7.75s worst case across 6 attempts, well inside every hook's
// 10-30s timeout in hooks/hooks.json. A real, sustained outage is the
// process supervisor's job (systemd/launchd's Restart=on-failure, see
// deploy/), not this retry loop's.
var connectRetryBackoff = []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// pingWithRetry pings db on the schedule in backoff (the first entry is
// always 0 — try immediately before ever sleeping), stopping early if ctx
// is canceled/expires. Returns the last ping error if every attempt fails.
func pingWithRetry(ctx context.Context, db *sql.DB, backoff []time.Duration) error {
	var err error
	for _, delay := range backoff {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
	}
	return err
}

// Open connects to dsn (a postgres:// URL) and ensures the schema exists.
// embedDims must match whatever embedding model the caller will use with
// SaveEmbedding — pass 0 to use DefaultEmbedDims.
func Open(ctx context.Context, dsn string, embedDims int) (*Store, error) {
	if embedDims <= 0 {
		embedDims = DefaultEmbedDims
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		// store.RedactDSN, not the raw dsn: a real Postgres DSN carries a
		// plaintext password, and this error is what every caller further
		// up the stack (cmd's various log sites, doctor, the worker
		// daemon, the MCP server) ends up logging or printing on any open
		// failure — a real credential leak this closes for every one of
		// them at once, not just here.
		return nil, fmt.Errorf("open postgres %s: %w", store.RedactDSN(dsn), err)
	}
	// Bounded, not left at database/sql's default of unlimited: every
	// long-lived process that talks to this backend (the worker daemon,
	// the MCP server) now opens exactly one Store for its whole lifetime
	// (see worker.Daemon.Run) rather than one per event — correct for
	// connection churn, but it means this pool is the ONLY thing standing
	// between a burst of concurrent calls and Postgres's own
	// max_connections limit, which every other client sharing the same
	// server also counts against. These are conservative defaults for a
	// hook-driven, not high-QPS, workload — not tuned against a real load
	// test, just deliberately bounded instead of silently unbounded.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := pingWithRetry(ctx, db, connectRetryBackoff); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	migrations := []migrate.Migration{
		{
			Version: 1,
			Name:    "initial observations table + indexes",
			Apply: func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx, fmt.Sprintf(schemaSQL, embedDims))
				return err
			},
		},
	}
	if err := migrate.Run(ctx, db, migrate.PostgresPlaceholder, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return &Store{db: db}, nil
}

func jsonEncode(items []string) []byte {
	if items == nil {
		items = []string{}
	}
	b, _ := json.Marshal(items)
	return b
}

func jsonDecode(raw []byte) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

// Insert mirrors store.Store's Insert exactly: content_hash is the
// idempotency key, ON CONFLICT DO NOTHING makes re-ingestion a no-op
// instead of a duplicate row. Postgres's database/sql driver has no
// LastInsertId support (there's no single generic "last id" concept the
// way SQLite's rowid gives one), so this uses RETURNING id directly instead.
func (s *Store) Insert(sessionID, project, toolName, contentHash string, o store.Observation, costUSD float64) (store.InsertResult, error) {
	now := time.Now()
	return s.insertRow(sessionID, project, toolName, contentHash, o, costUSD, now, now.UnixMilli())
}

// insertRow is Insert's and ImportRow's (export.go) shared implementation.
// Insert always passes time.Now() for createdAt/createdAtEpoch (both from
// the SAME clock read, not a Go timestamp paired with Postgres's own now()
// — the previous version relied on the created_at column's DEFAULT now(),
// a subtly different clock than the created_at_epoch value it computed in
// Go); ImportRow (a restore) passes the original values through instead,
// so a restore reflects when things actually happened.
func (s *Store) insertRow(sessionID, project, toolName, contentHash string, o store.Observation, costUSD float64, createdAt time.Time, createdAtEpoch int64) (store.InsertResult, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO observations
			(session_id, project, tool_name, type, title, subtitle, facts, narrative,
			 concepts, files_read, files_modified, cost_usd, created_at, created_at_epoch, content_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 ON CONFLICT (content_hash) DO NOTHING
		 RETURNING id`,
		sessionID, project, toolName, o.Type, o.Title, o.Subtitle,
		jsonEncode(o.Facts), o.Narrative, jsonEncode(o.Concepts),
		jsonEncode(o.FilesRead), jsonEncode(o.FilesModified),
		costUSD, createdAt, createdAtEpoch, contentHash,
	).Scan(&id)

	if err == sql.ErrNoRows {
		// ON CONFLICT DO NOTHING fired: no row to RETURN. Not an error —
		// look up the row that already holds this content_hash.
		if lookupErr := s.db.QueryRow(`SELECT id FROM observations WHERE content_hash = $1`, contentHash).Scan(&id); lookupErr != nil {
			return store.InsertResult{}, fmt.Errorf("look up existing observation for duplicate content_hash: %w", lookupErr)
		}
		return store.InsertResult{ID: id, Inserted: false}, nil
	}
	if err != nil {
		return store.InsertResult{}, fmt.Errorf("insert observation: %w", err)
	}
	return store.InsertResult{ID: id, Inserted: true}, nil
}

func (s *Store) CountByProject(project string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE project = $1`, project).Scan(&n)
	return n, err
}

func (s *Store) Close() error { return s.db.Close() }

// SaveEmbedding stores vec using pgvector's native type — dims must match
// the column's fixed dimensionality (see Open's embedDims), or Postgres
// rejects the write outright rather than silently truncating/padding.
func (s *Store) SaveEmbedding(observationID int64, vec []float32) error {
	_, err := s.db.Exec(`UPDATE observations SET embedding = $1 WHERE id = $2`,
		pgvector.NewVector(vec), observationID)
	if err != nil {
		return fmt.Errorf("save embedding for observation %d: %w", observationID, err)
	}
	return nil
}

// SemanticSearch orders by pgvector's cosine-distance operator (<=>),
// backed by the HNSW index from Open's schema — real ANN search, not a
// linear scan. Score is 1-distance so it matches the SQLite backend's
// convention (higher = closer, same range as cosine similarity).
//
// project scopes the comparison set to one project when non-empty, for the
// same cross-project-leak reason as Search.
func (s *Store) SemanticSearch(project string, queryVec []float32, limit int) ([]store.VectorMatch, error) {
	scope := ""
	args := []any{pgvector.NewVector(queryVec), limit}
	if project != "" {
		scope = "AND project = $3"
		args = append(args, project)
	}
	rows, err := s.db.Query(`
		SELECT id, project, tool_name, type, title, subtitle, facts, narrative,
		       concepts, files_read, files_modified, 1 - (embedding <=> $1) AS score
		FROM observations
		WHERE embedding IS NOT NULL `+scope+`
		ORDER BY embedding <=> $1
		LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	defer rows.Close()

	var out []store.VectorMatch
	for rows.Next() {
		var m store.VectorMatch
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&m.ID, &m.Project, &m.ToolName, &m.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified, &m.Score); err != nil {
			return nil, fmt.Errorf("scan semantic search row: %w", err)
		}
		nf.apply(&m.Observation)
		m.Observation.Facts = jsonDecode(facts)
		m.Observation.Concepts = jsonDecode(concepts)
		m.Observation.FilesRead = jsonDecode(filesRead)
		m.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, m)
	}
	return out, rows.Err()
}
