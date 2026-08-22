// Package sqlite is claude-mem-go's zero-dependency default memory.Backend:
// observations persisted to a single SQLite file with FTS5 keyword search
// and brute-force cosine semantic search. The storage-neutral types it
// implements against live in the parent memory package; the scale-up
// sibling is memory/postgres.
//
// The schema is a deliberately narrower cousin of claude-mem's real
// observations table (src/services/sqlite/SessionStore.ts), which has
// accumulated 40+ migrations' worth of columns (sync/origin-device
// bookkeeping, an FTS5 shadow table, content hashes, etc.). This keeps only
// the columns that describe an observation's actual content — the same
// ones ResponseProcessor.ts populates into its broadcast record — encoded
// the same way: facts/concepts/files_read/files_modified as
// JSON.stringify(x||[]) TEXT columns, not raw XML.
//
// IMPORTANT: this is its own database, in ~/.claude-mem-go/. It must never
// write to the real ~/.claude-mem/claude-mem.db — that file has a live
// foreign key (memory_session_id -> sdk_sessions) and schema this package
// doesn't replicate; sharing storage would risk corrupting a real claude-mem
// installation for no benefit.
package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	_ "modernc.org/sqlite" // pure-Go driver, no cgo — registers as "sqlite"
)

// Store is a thin wrapper around the sqlite handle.
type Store struct {
	db *sql.DB
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS observations (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id        TEXT NOT NULL,
	project           TEXT NOT NULL,
	tool_name         TEXT NOT NULL,
	type              TEXT NOT NULL,
	title             TEXT,
	subtitle          TEXT,
	facts             TEXT NOT NULL DEFAULT '[]',
	narrative         TEXT,
	concepts          TEXT NOT NULL DEFAULT '[]',
	next_steps        TEXT NOT NULL DEFAULT '[]',
	files_read        TEXT NOT NULL DEFAULT '[]',
	files_modified    TEXT NOT NULL DEFAULT '[]',
	cost_usd          REAL NOT NULL DEFAULT 0,
	created_at        TEXT NOT NULL,
	created_at_epoch  INTEGER NOT NULL
	-- content_hash is added by ensureContentHashColumn (dedup.go), not here:
	-- a brand-new database gets it as part of the same call, but a database
	-- created before this column existed needs ALTER TABLE, not CREATE TABLE
	-- IF NOT EXISTS (which is a no-op once the table already exists).
);
CREATE INDEX IF NOT EXISTS idx_observations_session ON observations(session_id);
CREATE INDEX IF NOT EXISTS idx_observations_project ON observations(project);
CREATE INDEX IF NOT EXISTS idx_observations_type ON observations(type);
CREATE INDEX IF NOT EXISTS idx_observations_created ON observations(created_at_epoch DESC);
`

// sqliteDSNParams are modernc.org/sqlite's shorthand DSN query params,
// applied on every physical connection the pool opens — not a one-time
// PRAGMA Exec call, which would only ever reach whichever single pooled
// connection happened to run it, leaving any other connection the pool
// opens later unconfigured. Two real, reproduced problems, both fixed by
// this:
//
//   - Without _journal_mode=WAL (+_busy_timeout), two genuinely concurrent
//     writers on the same file — exactly this project's real shape, since
//     the worker daemon and every CLI subcommand each open their own
//     *sql.DB against the same file — hit "database is locked" (SQLITE_BUSY)
//     immediately under the default rollback-journal mode's exclusive write
//     lock. Reproduced directly: one connection holding an open write
//     transaction made a second connection's INSERT fail outright.
//   - Without _foreign_keys=on, observation_vectors' ON DELETE CASCADE
//     never fires — SQLite's foreign-key enforcement defaults to OFF, full
//     stop, regardless of the schema declaring the constraint. Reproduced
//     directly: Prune-ing an observation with a saved embedding left its
//     observation_vectors row behind, orphaned, forever.
const sqliteDSNParams = "_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"

// Open opens (creating if needed) the sqlite file at path and ensures the
// schema exists.
func Open(path string) (*Store, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	db, err := sql.Open("sqlite", path+sep+sqliteDSNParams)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return &Store{db: db}, nil
}

func jsonArray(items []string) string {
	if items == nil {
		items = []string{}
	}
	b, _ := json.Marshal(items) // []string always marshals cleanly
	return string(b)
}

// Insert persists one observation, JSON-encoding facts/concepts/files_*
// exactly like ResponseProcessor.ts's broadcast record does. contentHash
// (see memory.ContentHash) is the idempotency key: inserting the same hash twice
// is a no-op that returns the original row, not a duplicate.
func (s *Store) Insert(sessionID, project, toolName, contentHash string, o memory.Observation, costUSD float64) (memory.InsertResult, error) {
	now := time.Now()
	return s.insertRow(sessionID, project, toolName, contentHash, o, costUSD, now.Format(time.RFC3339), now.UnixMilli())
}

// insertRow is Insert's and ImportRow's (export.go) shared implementation —
// the only difference between "capture a new observation now" and "restore
// a previously exported one" is which created_at/created_at_epoch gets
// written, so that's the one thing this takes as parameters rather than
// always stamping time.Now() itself.
func (s *Store) insertRow(sessionID, project, toolName, contentHash string, o memory.Observation, costUSD float64, createdAt string, createdAtEpoch int64) (memory.InsertResult, error) {
	if err := memory.ValidateObservationType(o.Type); err != nil {
		return memory.InsertResult{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO observations
			(session_id, project, tool_name, type, title, subtitle, facts, narrative,
			 concepts, files_read, files_modified, next_steps, cost_usd, created_at, created_at_epoch, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(content_hash) DO NOTHING`,
		sessionID, project, toolName, o.Type, o.Title, o.Subtitle,
		jsonArray(o.Facts), o.Narrative, jsonArray(o.Concepts),
		jsonArray(o.FilesRead), jsonArray(o.FilesModified), jsonArray(o.NextSteps),
		costUSD, createdAt, createdAtEpoch, contentHash,
	)
	if err != nil {
		return memory.InsertResult{}, fmt.Errorf("insert observation: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return memory.InsertResult{}, fmt.Errorf("insert observation, checking rows affected: %w", err)
	}
	if affected == 0 {
		var id int64
		if err := s.db.QueryRow(`SELECT id FROM observations WHERE content_hash = ?`, contentHash).Scan(&id); err != nil {
			return memory.InsertResult{}, fmt.Errorf("look up existing observation for duplicate content_hash: %w", err)
		}
		return memory.InsertResult{ID: id, Inserted: false}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return memory.InsertResult{}, err
	}
	return memory.InsertResult{ID: id, Inserted: true}, nil
}

// CountByProject is a small read-path check useful for verifying a round
// trip, mirroring the kind of query src/services/sqlite/SessionSearch.ts
// runs (filter by project).
func (s *Store) CountByProject(project string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE project = ?`, project).Scan(&n)
	return n, err
}

func (s *Store) Close() error { return s.db.Close() }
