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

// Open connects to dsn (a postgres:// URL) and ensures the schema exists.
// embedDims must match whatever embedding model the caller will use with
// SaveEmbedding — pass 0 to use DefaultEmbedDims.
func Open(ctx context.Context, dsn string, embedDims int) (*Store, error) {
	if embedDims <= 0 {
		embedDims = DefaultEmbedDims
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres %s: %w", dsn, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(schemaSQL, embedDims)); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
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
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO observations
			(session_id, project, tool_name, type, title, subtitle, facts, narrative,
			 concepts, files_read, files_modified, cost_usd, created_at_epoch, content_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 ON CONFLICT (content_hash) DO NOTHING
		 RETURNING id`,
		sessionID, project, toolName, o.Type, o.Title, o.Subtitle,
		jsonEncode(o.Facts), o.Narrative, jsonEncode(o.Concepts),
		jsonEncode(o.FilesRead), jsonEncode(o.FilesModified),
		costUSD, time.Now().UnixMilli(), contentHash,
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
func (s *Store) SemanticSearch(queryVec []float32, limit int) ([]store.VectorMatch, error) {
	rows, err := s.db.Query(`
		SELECT id, project, tool_name, type, title, subtitle, facts, narrative,
		       concepts, files_read, files_modified, 1 - (embedding <=> $1) AS score
		FROM observations
		WHERE embedding IS NOT NULL
		ORDER BY embedding <=> $1
		LIMIT $2`, pgvector.NewVector(queryVec), limit)
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
