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
	"net/url"
	"os"
	"strconv"
	"strings"
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

// hnswEfSearchMin and hnswEfSearchMax are pgvector's own hard-coded bounds
// on hnsw.ef_search, confirmed by hand against a real container (values
// outside this range fail with "N is outside the valid range for
// parameter \"hnsw.ef_search\" (1 .. 1000)"). Validated here at Open time
// rather than left for Postgres to reject at query time, because Postgres
// itself doesn't reject it reliably: hnsw.ef_search is a custom GUC
// pgvector's extension registers, and a real, reproducible quirk found by
// hand shows Postgres only enforces its bounds once something in that
// specific backend connection has already touched the vector extension
// (any real vector operation) — an otherwise-idle connection accepts an
// out-of-range SET LOCAL silently, with no error at all, because Postgres
// treats an as-yet-unregistered custom GUC name as an unchecked
// placeholder. Reproduced directly: identical Go code (BeginTx → SET
// LOCAL 1001 → Commit) errored correctly through a connection warmed by a
// prior real Insert/SaveEmbedding call, but silently accepted the same
// invalid value on an otherwise-idle fresh connection whose first-ever
// query was that SET LOCAL — a real Postgres/pgvector connection-state
// dependency, not a hypothetical edge case, and one a caller configuring
// this flag would have no reliable way to detect without this check.
const (
	hnswEfSearchMin = 1
	hnswEfSearchMax = 1000
)

// Store is the Postgres-backed store.Backend implementation.
type Store struct {
	db *sql.DB
	// hnswEfSearch, when > 0, overrides pgvector's own hnsw.ef_search
	// default (40) for every SemanticSearch call — see SemanticSearch's
	// own doc comment for why this needs a transaction-scoped SET LOCAL
	// rather than a plain SET against a pooled connection. 0 (Open's
	// default) leaves pgvector's built-in default in place untouched.
	hnswEfSearch int
	// iterativeScan records whether this server's pgvector supports
	// hnsw.iterative_scan (0.8.0+). Detected once at Open rather than
	// probed per call: an unsupported hnsw.* GUC does not fail
	// harmlessly — on a connection that has already touched a vector
	// operation it errors outright ("invalid configuration parameter
	// name", "hnsw is a reserved prefix"), which would take
	// SemanticSearch down entirely on an older pgvector. Confirmed by
	// hand, and consistent with the cold-vs-warm connection quirk this
	// package already documents for hnsw.ef_search.
	iterativeScan bool
	// embedDims is the observations.embedding column's ACTUAL width,
	// read back from the catalog at Open rather than assumed — see
	// columnEmbedDims for why the configured value and the real one can
	// legitimately differ.
	embedDims int
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
	-- facts/concepts are indexed here too, at weight 'D'. Leaving them out
	-- (as this originally did) was a real, measured cross-backend
	-- divergence, not a theoretical one: the SQLite backend's FTS5 table
	-- has always covered all five columns, so the same observation was
	-- keyword-searchable on one store.Backend implementation and invisible
	-- on the other. Real claude-mem indexes them on both its engines too
	-- (its SQLite fts5 lists facts/concepts explicitly; its Postgres
	-- content_search tsvectors the entire observation content).
	--
	-- ::text on a jsonb column is immutable enough for a STORED generated
	-- column — verified against a real Postgres 16, which rejects
	-- non-immutable expressions here outright — and JSON's own brackets and
	-- quotes tokenize away harmlessly ('["multi-agent system"]' yields
	-- 'agent':3 'multi':2 'multi-ag':1 'system':4).
	--
	-- Weight 'D' keeps ts_rank_cd ordering preferring a title or narrative
	-- hit over a tag hit, rather than letting a concepts match outrank the
	-- observation's own headline.
	search_vector     tsvector GENERATED ALWAYS AS (
		setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
		setweight(to_tsvector('english', coalesce(subtitle, '')), 'B') ||
		setweight(to_tsvector('english', coalesce(narrative, '')), 'C') ||
		setweight(to_tsvector('english', coalesce(facts::text, '')), 'D') ||
		setweight(to_tsvector('english', coalesce(concepts::text, '')), 'D')
	) STORED
);

CREATE INDEX IF NOT EXISTS idx_observations_project ON observations(project);
CREATE INDEX IF NOT EXISTS idx_observations_session ON observations(session_id);
CREATE INDEX IF NOT EXISTS idx_observations_type ON observations(type);
CREATE INDEX IF NOT EXISTS idx_observations_created ON observations(created_at_epoch DESC);

-- GIN index over the generated tsvector — real full-text search, not a
-- hand-rolled shadow table.
CREATE INDEX IF NOT EXISTS idx_observations_search_vector ON observations USING GIN(search_vector);

-- GIN over the file arrays. ObservationsForFile runs before EVERY Read
-- tool call (the PreToolUse file-context hook), and without these the
-- key-exists containment test is a post-filter over every row in the
-- project: measured at 250,000 rows with realistic file arrays, 47.7ms
-- without vs 12.9ms with, scaling with rows-per-project rather than with
-- matches. Default jsonb_ops, not jsonb_path_ops, because the query uses
-- the key-exists operator, which jsonb_path_ops does not support -- it
-- would index fine and then never be used.
CREATE INDEX IF NOT EXISTS idx_observations_files_read ON observations USING GIN (files_read);
CREATE INDEX IF NOT EXISTS idx_observations_files_modified ON observations USING GIN (files_modified);

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
// hang: with defaultConnectionTimeoutMS bounding each individual attempt
// (see pingWithRetry), worst case is that bound × 6 attempts plus the
// backoff spacing below — a firewalled/black-holed host no longer hangs
// on the OS's own TCP connect timeout (commonly 75s+, sometimes
// effectively unbounded) on every one of the 6 attempts, the failure mode
// a bare db.PingContext(ctx) against an undeadlined caller context was
// still exposed to before that per-attempt bound existed. A real,
// sustained outage is still the process supervisor's job (systemd/
// launchd's Restart=on-failure, see deploy/), not this retry loop's.
var connectRetryBackoff = []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// defaultConnectionTimeoutMS matches real claude-mem's own Postgres pool
// default exactly (src/storage/postgres/config.ts's
// DEFAULT_CONNECTION_TIMEOUT_MS), applied via the identical env var name
// (connectionTimeoutEnvVar) for the same cross-system-migration reason
// defaultStatementTimeoutMS documents. Unlike statement_timeout (a DSN
// parameter Postgres itself enforces once connected), this bounds the
// TCP-connect phase itself — pgx has no equivalent DSN knob for that, so
// it's applied as a Go-side context.WithTimeout around each individual
// ping attempt instead (see pingWithRetry).
const defaultConnectionTimeoutMS = 5_000

const connectionTimeoutEnvVar = "CLAUDE_MEM_POSTGRES_CONNECTION_TIMEOUT_MS"

func connectionTimeout() time.Duration {
	ms := defaultConnectionTimeoutMS
	if v := os.Getenv(connectionTimeoutEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// defaultPoolMax and poolMaxEnvVar match real claude-mem's own
// DEFAULT_POOL_MAX/CLAUDE_MEM_POSTGRES_POOL_MAX exactly — an operator
// sharing one Postgres server across many deployments (the same scenario
// this project's own MaxOpenConns doc comment above discusses) had no
// way to tune this port's pool size at all before this, unlike every
// other knob real claude-mem's config.ts exposes.
const defaultPoolMax = 10

const poolMaxEnvVar = "CLAUDE_MEM_POSTGRES_POOL_MAX"

func poolMax() int {
	n := defaultPoolMax
	if v := os.Getenv(poolMaxEnvVar); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	return n
}

// defaultIdleTimeoutMS and idleTimeoutEnvVar match real claude-mem's own
// DEFAULT_IDLE_TIMEOUT_MS/CLAUDE_MEM_POSTGRES_IDLE_TIMEOUT_MS exactly. A
// real, found-by-hand mismatch this replaces: Open used to hardcode
// SetConnMaxIdleTime to 5 minutes — a 10x mismatch against real
// claude-mem's own 30-SECOND default, with no override path at all.
const defaultIdleTimeoutMS = 30_000

const idleTimeoutEnvVar = "CLAUDE_MEM_POSTGRES_IDLE_TIMEOUT_MS"

func idleTimeout() time.Duration {
	ms := defaultIdleTimeoutMS
	if v := os.Getenv(idleTimeoutEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// pingWithRetry pings db on the schedule in backoff (the first entry is
// always 0 — try immediately before ever sleeping), stopping early if ctx
// is canceled/expires. Returns the last ping error if every attempt fails.
//
// Each individual attempt is bounded by connectionTimeout(), not ctx
// directly — a real gap found by hand: every real caller in this project
// passes an undeadlined context (see connectRetryBackoff's own doc
// comment), so db.PingContext(ctx) alone could hang on a single attempt
// for however long the OS's own TCP connect timeout is against a
// firewalled or black-holed host — the realistic "Postgres unreachable"
// case, not just "container still starting." context.WithTimeout
// composes correctly with whatever deadline ctx itself might already
// carry (the shorter of the two always wins), so this only ever tightens
// the bound, never loosens a caller's own tighter deadline.
func pingWithRetry(ctx context.Context, db *sql.DB, backoff []time.Duration) error {
	timeout := connectionTimeout()
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
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		err = db.PingContext(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
	}
	return err
}

// defaultStatementTimeoutMS matches real claude-mem's own Postgres pool
// default (src/storage/postgres/config.ts's DEFAULT_STATEMENT_TIMEOUT_MS)
// exactly, applied via the identical env var name
// (statementTimeoutEnvVar) so an operator migrating settings between the
// two doesn't need to learn a new knob name.
const defaultStatementTimeoutMS = 30_000

const statementTimeoutEnvVar = "CLAUDE_MEM_POSTGRES_STATEMENT_TIMEOUT_MS"

// withStatementTimeout returns dsn with a statement_timeout query
// parameter appended, so pgx applies it as a startup runtime parameter
// on every physical connection this DSN ever opens — the direct
// equivalent of real claude-mem's own statement_timeout pool option
// (createPostgresPool, which the "pg" driver applies identically on
// connect). Without this, nothing here ever bounded how long a single
// query could run: MaxOpenConns caps this pool at 10 connections total,
// shared by every hook process and the worker daemon, and a single
// query that hangs (lock contention from a concurrent prune/reembed, a
// pathological query plan, a network stall) would hold its connection
// forever — a handful of stuck queries exhausts the whole pool and
// every subsequent caller blocks indefinitely with no self-healing.
//
// Left untouched if the DSN already specifies statement_timeout
// explicitly (an operator's own choice wins), or if the DSN doesn't
// parse as a URL at all — in the latter case sql.Open/pingWithRetry
// fail on their own shortly after, same as any other malformed DSN;
// silently swallowing that error here to force a timeout in would be
// worse than just letting the real failure surface.
//
// Confirmed empirically against a real container, not assumed from
// pgx's own docs: a DSN with statement_timeout=2000 appended, run
// against a real `SELECT pg_sleep(5)`, errored at ~2s with Postgres's
// own "canceling statement due to statement timeout" (SQLSTATE 57014)
// rather than the parameter being silently ignored.
func withStatementTimeout(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	if q.Get("statement_timeout") != "" {
		return dsn
	}
	ms := defaultStatementTimeoutMS
	if v := os.Getenv(statementTimeoutEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	q.Set("statement_timeout", strconv.Itoa(ms))
	u.RawQuery = q.Encode()
	return u.String()
}

// Open connects to dsn (a postgres:// URL) and ensures the schema exists.
// embedDims must match whatever embedding model the caller will use with
// SaveEmbedding — pass 0 to use DefaultEmbedDims. hnswEfSearch overrides
// pgvector's own hnsw.ef_search default for every SemanticSearch call
// against the returned Store — pass 0 to leave pgvector's built-in
// default (40) in place, same "0 means use the default" convention as
// embedDims. A nonzero value outside pgvector's own 1..1000 range fails
// immediately here, deterministically — see hnswEfSearchMin/Max's own doc
// comment for why this can't be left for Postgres to reject at query time.
func Open(ctx context.Context, dsn string, embedDims, hnswEfSearch int) (*Store, error) {
	if embedDims <= 0 {
		embedDims = configuredEmbedDims(DefaultEmbedDims)
	}
	if hnswEfSearch != 0 && (hnswEfSearch < hnswEfSearchMin || hnswEfSearch > hnswEfSearchMax) {
		return nil, fmt.Errorf("hnsw.ef_search must be between %d and %d (or 0 to use pgvector's default), got %d",
			hnswEfSearchMin, hnswEfSearchMax, hnswEfSearch)
	}
	dsn = withStatementTimeout(dsn)
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
	// server also counts against. poolMax()/idleTimeout() match real
	// claude-mem's own pool defaults exactly and are overridable via the
	// identical env var names — these are conservative defaults for a
	// hook-driven, not high-QPS, workload, not tuned against a real load
	// test, just deliberately bounded instead of silently unbounded.
	db.SetMaxOpenConns(poolMax())
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(idleTimeout())
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
		{
			// A real schema-completeness gap, found by hand, not a
			// demonstrated live bug: nothing anywhere validated `type`
			// against this project's own small, fixed vocabulary
			// (discovery/change/decision/summary/manual — see
			// store.ValidObservationTypes) before this — not the schema,
			// not the Go code. Go-side validation now exists at every
			// real ingestion path (store.ValidateObservationType, called
			// from insertRow in both backends), but a real CHECK
			// constraint is the stronger, schema-level guarantee this
			// production-scale backend's own "real ANN vector search...
			// at scale" mandate implies — enforced regardless of which
			// code path ever writes a row, not just the ones that
			// happen to go through this Go package.
			//
			// Confirmed safe to add against a real, long-lived, shared
			// database before writing this: queried the actual
			// distinct `type` values across 3000+ real rows this
			// project's own testing accumulated in its dev container,
			// and every single one already fell within this vocabulary
			// — zero drift, so this closes a real gap without failing
			// to apply against data that already exists.
			//
			// Wrapped in a DO block with an existence check rather than
			// a bare ALTER TABLE ADD CONSTRAINT: Postgres has no
			// `ADD CONSTRAINT IF NOT EXISTS`, and migrate.Run's own
			// idempotency contract (see migrate's package doc) requires
			// Apply to tolerate being invoked again — a plain ALTER
			// TABLE would fail with "constraint already exists" on any
			// re-run.
			Version: 2,
			Name:    "CHECK constraint on observations.type",
			Apply: func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx, `
					DO $$
					BEGIN
						IF NOT EXISTS (
							SELECT 1 FROM pg_constraint WHERE conname = 'observations_type_check'
						) THEN
							ALTER TABLE observations ADD CONSTRAINT observations_type_check
								CHECK (type IN ('discovery', 'change', 'decision', 'summary', 'manual'));
						END IF;
					END $$;
				`)
				return err
			},
		},
		{
			// Rebuilds search_vector to also cover facts and concepts —
			// see schemaSQL's own comment for why they belong there and
			// why ::text/'D' are the right choices. A generated column's
			// expression can't be ALTERed in place, so the column is
			// dropped and re-added; Postgres recomputes it for every
			// existing row on ADD COLUMN, so this backfills the whole
			// table rather than only helping new writes.
			//
			// The index has to go first: DROP COLUMN would take it along
			// anyway, but dropping it explicitly keeps the intent obvious
			// and the re-CREATE below symmetric. Every statement is
			// IF EXISTS / IF NOT EXISTS so a partially-applied run (this
			// is the one migration here heavy enough to plausibly be
			// interrupted on a large table) re-runs cleanly, as
			// migrate.Run requires of every migration.
			Version: 3,
			Name:    "index facts and concepts in search_vector",
			Apply: func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx, `
					DROP INDEX IF EXISTS idx_observations_search_vector;
					ALTER TABLE observations DROP COLUMN IF EXISTS search_vector;
					ALTER TABLE observations ADD COLUMN search_vector tsvector GENERATED ALWAYS AS (
						setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
						setweight(to_tsvector('english', coalesce(subtitle, '')), 'B') ||
						setweight(to_tsvector('english', coalesce(narrative, '')), 'C') ||
						setweight(to_tsvector('english', coalesce(facts::text, '')), 'D') ||
						setweight(to_tsvector('english', coalesce(concepts::text, '')), 'D')
					) STORED;
					CREATE INDEX IF NOT EXISTS idx_observations_search_vector ON observations USING GIN(search_vector);
				`)
				return err
			},
		},
		{
			Version: 4,
			Name:    "GIN-index files_read and files_modified",
			Apply: func(ctx context.Context, db *sql.DB) error {
				// ObservationsForFile is the PreToolUse file-context read,
				// so it runs before EVERY Read tool call — the highest
				// frequency query this backend serves. It had no usable
				// index: `files_read ? $2 OR files_modified ? $2` was a
				// post-filter, so the project index narrowed to that
				// project's rows and then every one of them was checked
				// by jsonb containment. Cost therefore scaled with rows
				// per project rather than with matches.
				//
				// Measured on a real 250,000-row corpus with realistic
				// file arrays (5,000 rows in the queried project, 313
				// genuine matches): 47.7ms without these indexes,
				// 12.9ms with them — and the plan changes from
				// "Rows Removed by Filter: 4687" to a BitmapOr over both
				// GIN indexes. A long-lived project with ten times the
				// history pays ten times the un-indexed cost, on every
				// file read.
				//
				// Default jsonb_ops, NOT jsonb_path_ops: the query uses
				// the `?` key-exists operator, which jsonb_path_ops does
				// not support — indexing with it would build successfully
				// and then never be used.
				_, err := db.ExecContext(ctx, `
					CREATE INDEX IF NOT EXISTS idx_observations_files_read
						ON observations USING GIN (files_read);
					CREATE INDEX IF NOT EXISTS idx_observations_files_modified
						ON observations USING GIN (files_modified);
				`)
				return err
			},
		},
	}
	if err := migrate.Run(ctx, db, migrate.PostgresPlaceholder, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	// The column's real width, not the requested one: on an existing
	// store they differ whenever the store predates the current
	// configuration, and every dimension check downstream must be
	// against what the column will actually accept.
	actualDims := columnEmbedDims(ctx, db)
	if actualDims == 0 {
		actualDims = embedDims
	}
	return &Store{db: db, hnswEfSearch: hnswEfSearch,
		iterativeScan: supportsIterativeScan(ctx, db), embedDims: actualDims}, nil
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
	if err := store.ValidateObservationType(o.Type); err != nil {
		return store.InsertResult{}, err
	}
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
	// Checked here, with both numbers and the remedy named, rather than
	// left to pgvector's bare "expected 768 dimensions, not 384" — which
	// says nothing about WHY they differ or what to do. The store's width
	// is fixed at creation, so this is a configuration mismatch (an embed
	// model whose output size doesn't match the store), not a transient
	// failure worth retrying.
	if s.embedDims > 0 && len(vec) != s.embedDims {
		return fmt.Errorf("save embedding for observation %d: this store's embedding column is vector(%d) but the model produced %d dimensions — "+
			"the column's width is fixed when the store is created, so either use an embedding model that emits %d dimensions or create a new store with %s=%d and re-embed",
			observationID, s.embedDims, len(vec), s.embedDims, defaultEmbedDimsEnvVar, len(vec))
	}
	res, err := s.db.Exec(`UPDATE observations SET embedding = $1 WHERE id = $2`,
		pgvector.NewVector(vec), observationID)
	if err != nil {
		return fmt.Errorf("save embedding for observation %d: %w", observationID, err)
	}
	// An UPDATE matching no rows is not success. The SQLite backend
	// stores embeddings in a separate table with a foreign key, so the
	// same call there fails loudly with a constraint violation; this one
	// returned nil, telling the caller an embedding was saved when
	// nothing was written at all. Measured divergence, not theoretical.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("save embedding for observation %d: no such observation", observationID)
	}
	return nil
}

// defaultEmbedDimsEnvVar lets an operator create a NEW Postgres store
// sized for an embedding model other than the default nomic-embed-text
// (768) — all-minilm is 384, mxbai-embed-large is 1024. An env var
// rather than a flag deliberately: this is a storage-layer knob in the
// same family as the pool settings above, every one of which is
// configured this way, and threading a new flag through all nine
// commands that expose -embed-model would put the setting in nine places
// instead of one.
//
// It only applies when the table is being CREATED. An existing store's
// column width is whatever it already is (see columnEmbedDims): a
// pgvector column's dimensionality is fixed at creation, so re-sizing an
// existing store means rewriting every vector, which is a deliberate
// migration rather than something a startup flag should do silently.
const defaultEmbedDimsEnvVar = "CLAUDE_MEM_POSTGRES_EMBED_DIMS"

func configuredEmbedDims(fallback int) int {
	if v := os.Getenv(defaultEmbedDimsEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// columnEmbedDims reads the real width of observations.embedding from
// the catalog. Returns 0 when the table or column doesn't exist yet
// (a fresh database, before migrations run).
//
// This is read rather than assumed because the configured value and the
// stored one genuinely diverge in practice: the column is created once
// by migration v1 and CREATE TABLE IF NOT EXISTS can never widen it
// afterwards, so a store created at 768 stays 768 no matter what any
// later configuration says. Knowing the real number is what lets
// SaveEmbedding fail with an actionable message instead of pgvector's
// bare "expected 768 dimensions, not 384".
func columnEmbedDims(ctx context.Context, db *sql.DB) int {
	var typmod int
	err := db.QueryRowContext(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = to_regclass('observations') AND attname = 'embedding'`).Scan(&typmod)
	if err != nil || typmod <= 0 {
		return 0
	}
	return typmod
}

// supportsIterativeScan reports whether this server's pgvector is new
// enough for hnsw.iterative_scan (0.8.0+), which SemanticSearch needs to
// filter by project without silently losing results. Any uncertainty —
// unreadable version, unparseable version — is reported as false so the
// caller takes the slower but always-correct path; guessing "supported"
// and being wrong would break SemanticSearch outright rather than merely
// slow it down.
func supportsIterativeScan(ctx context.Context, db *sql.DB) bool {
	var version string
	if err := db.QueryRowContext(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&version); err != nil {
		return false
	}
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > 0 || minor >= 8
}

// semanticSearchPlan decides how a SemanticSearch call must be executed
// so that a project filter can never starve the candidate set — see
// SemanticSearch's own comment for the measured bug this guards.
//
// Split out as its own method purely so the routing is testable without
// the tens of thousands of rows it takes to make Postgres's planner
// actually choose the HNSW plan. That threshold is why the bug survived
// this long: at small row counts the planner picks a plain btree
// pre-filter and returns correct results, so no small-scale test could
// have caught it.
//
// An unscoped search (project == "") needs neither treatment: with no
// filter there is nothing for the post-filter to discard.
func (s *Store) semanticSearchPlan(project string) (useCTE, useIterative bool) {
	if project == "" {
		return false, false
	}
	if s.iterativeScan {
		return false, true
	}
	return true, false
}

// SemanticSearch orders by pgvector's cosine-distance operator (<=>),
// backed by the HNSW index from Open's schema — real ANN search, not a
// linear scan. Score is 1-distance so it matches the SQLite backend's
// convention (higher = closer, same range as cosine similarity).
//
// project scopes the comparison set to one project when non-empty, for the
// same cross-project-leak reason as Search.
func (s *Store) SemanticSearch(project string, queryVec []float32, limit int) ([]store.VectorMatch, error) {
	limit = clampNegativeLimit(limit)
	scope := ""
	args := []any{pgvector.NewVector(queryVec), limit}
	if project != "" {
		scope = "AND project = $3"
		args = append(args, project)
	}
	const cols = `id, project, tool_name, type, title, subtitle, facts, narrative,
		       concepts, files_read, files_modified`
	query := `
		SELECT ` + cols + `, 1 - (embedding <=> $1) AS score
		FROM observations
		WHERE embedding IS NOT NULL ` + scope + `
		ORDER BY embedding <=> $1
		LIMIT $2`

	// The project filter above is a POST-filter on the HNSW scan, and
	// that is a real, measured bug, not a theoretical one: pgvector walks
	// hnsw.ef_search (default 40) globally-nearest candidates and only
	// then drops the ones whose project doesn't match, so when a
	// project's rows aren't among the global nearest, every candidate is
	// discarded and the result is EMPTY — no error, no warning.
	// Reproduced through this exact Go API against a real container with
	// the real schema and every index in place: 60,000 embedded rows in
	// the queried project returned 0 matches, with EXPLAIN showing
	// "Index Scan using idx_observations_embedding_hnsw ... Rows Removed
	// by Filter: 40". That turns this backend's whole reason for existing
	// — one shared Postgres serving many projects — into silence, and it
	// feeds the UserPromptSubmit hook and the semantic MCP tools, so
	// memory simply stops answering.
	//
	// pgvector 0.8's hnsw.iterative_scan is the purpose-built fix: the
	// scan keeps going until enough rows survive the filter. Measured on
	// the same 60k-row reproduction, it returns the correct 10 rows in
	// ~26ms. strict_order rather than relaxed_order because callers
	// present these as ranked results and relaxed_order explicitly gives
	// up ordering guarantees. A materialized CTE would also fix it and be
	// exact, but measured 2.7s on that same data versus ~26ms — it throws
	// away the ANN property this backend exists to provide, so it's the
	// fallback for older pgvector only, not the default.
	useCTE, useIterative := s.semanticSearchPlan(project)
	scoped := project != ""
	if !scoped && s.hnswEfSearch <= 0 {
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("semantic search: %w", err)
		}
		defer rows.Close()
		return scanVectorMatches(rows)
	}
	if useCTE {
		// Older pgvector: no iterative scan available, so pre-filter in a
		// MATERIALIZED CTE. Exact and always correct, but O(rows in
		// project) — slow-and-right beats fast-and-silently-empty.
		query = `
		WITH scoped AS MATERIALIZED (
			SELECT ` + cols + `, embedding
			FROM observations
			WHERE embedding IS NOT NULL ` + scope + `
		)
		SELECT ` + cols + `, 1 - (embedding <=> $1) AS score
		FROM scoped
		ORDER BY embedding <=> $1
		LIMIT $2`
		if s.hnswEfSearch <= 0 {
			rows, err := s.db.Query(query, args...)
			if err != nil {
				return nil, fmt.Errorf("semantic search: %w", err)
			}
			defer rows.Close()
			return scanVectorMatches(rows)
		}
	}

	// SET LOCAL, not a plain SET: s.db is a pooled *sql.DB, and
	// database/sql gives no control over which physical connection any
	// one call gets — a plain SET would apply to whatever connection
	// happens to serve THIS call and then silently persist for whatever
	// UNRELATED query the pool hands that same connection next. SET LOCAL
	// confines every override below to this one transaction, gone the
	// instant it ends.
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("begin semantic search tx: %w", err)
	}
	defer tx.Rollback()
	if s.hnswEfSearch > 0 {
		// hnsw.ef_search controls the ANN index's query-time
		// recall/speed tradeoff — pgvector's built-in default (40)
		// doesn't necessarily hold as `observations` grows well past the
		// row counts it was tuned against.
		if _, err := tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", s.hnswEfSearch)); err != nil {
			return nil, fmt.Errorf("set hnsw.ef_search: %w", err)
		}
	}
	if useIterative {
		if _, err := tx.Exec("SET LOCAL hnsw.iterative_scan = strict_order"); err != nil {
			return nil, fmt.Errorf("set hnsw.iterative_scan: %w", err)
		}
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	out, err := scanVectorMatches(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func scanVectorMatches(rows *sql.Rows) ([]store.VectorMatch, error) {
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
