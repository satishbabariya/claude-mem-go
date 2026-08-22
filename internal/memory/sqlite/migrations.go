package sqlite

import (
	"context"
	"database/sql"

	"github.com/satishbabariya/claude-mem-go/internal/migrate"
)

// migrations is every schema change this backend has ever made, in the
// order they must apply. Each Apply must stay idempotent on its own (see
// migrate's package doc) — these four already were, from before this list
// existed; this just gives them a version number and a recorded history
// instead of running unconditionally (or via a bespoke existence check)
// every time Open is called.
var migrations = []migrate.Migration{
	{
		Version: 1,
		Name:    "initial observations table",
		Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, createTableSQL)
			return err
		},
	},
	{
		Version: 2,
		Name:    "content_hash dedup column",
		Apply: func(ctx context.Context, db *sql.DB) error {
			return ensureContentHashColumn(db)
		},
	},
	{
		Version: 3,
		Name:    "fts5 keyword search index",
		Apply: func(ctx context.Context, db *sql.DB) error {
			return ensureFTS(db)
		},
	},
	{
		Version: 4,
		Name:    "observation_vectors embedding table",
		Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, createVectorTableSQL)
			return err
		},
	},
	{
		// A real bug, not a defensive fix: migration 3's original trigger
		// used the fts5 "special command" delete form, valid only for
		// contentless/external-content tables — this table is neither. It
		// went unnoticed because nothing ever deleted from `observations`
		// until Prune (prune.go). A database that already ran migration 3
		// has the broken trigger recorded as applied, so createFTSSQL's own
		// `CREATE TRIGGER IF NOT EXISTS` (already fixed) is a no-op against
		// it — this migration explicitly drops and recreates it to actually
		// reach every existing database, not just new ones.
		Version: 5,
		Name:    "fix fts5 delete trigger (was using external-content-only syntax)",
		Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `
				DROP TRIGGER IF EXISTS observations_ad;
				CREATE TRIGGER IF NOT EXISTS observations_ad AFTER DELETE ON observations BEGIN
					DELETE FROM observations_fts WHERE rowid = old.id;
				END;
			`)
			return err
		},
	},
	{
		// ObservationsForFile is the PreToolUse file-context read, and
		// that hook is async:false with a 10s timeout — it BLOCKS the
		// Read tool call, so its cost is latency the user actually waits
		// through, on every single file read.
		//
		// It had no usable index. `EXISTS (SELECT 1 FROM json_each(...))`
		// cannot use one: the project index narrows to that project's
		// rows and then json_each is run over every one of them, twice.
		// Measured on a real 50,000-row single-project SQLite store:
		// 46.8ms for a matching file, 46.2ms for one matching nothing
		// (the cost is the scan, not the result), and 94.3ms for a path
		// every row shares. It grows linearly with project size.
		//
		// Postgres solved the same query with GIN indexes on the jsonb
		// columns (migration 4 there). SQLite has no equivalent — you
		// cannot index the output of a table-valued function — so the
		// paths are denormalized into a real indexed table.
		//
		// Kept in sync by trigger rather than from Go, deliberately: it
		// then holds for every write path that exists or ever will
		// (Insert, ImportRow, anything else), which the FTS index here
		// already does the same way. Deletes are handled by ON DELETE
		// CASCADE, which actually fires because the DSN sets
		// _foreign_keys=on — see sqliteDSNParams, where that is already
		// load-bearing for observation_vectors.
		Version: 6,
		Name:    "index observation file paths for the file-context read",
		Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS observation_files (
					observation_id INTEGER NOT NULL REFERENCES observations(id) ON DELETE CASCADE,
					path TEXT NOT NULL,
					PRIMARY KEY (observation_id, path)
				);
				CREATE INDEX IF NOT EXISTS idx_observation_files_path ON observation_files(path);

				DROP TRIGGER IF EXISTS observation_files_ai;
				CREATE TRIGGER observation_files_ai AFTER INSERT ON observations BEGIN
					INSERT OR IGNORE INTO observation_files(observation_id, path)
						SELECT new.id, value FROM json_each(new.files_read)
						UNION
						SELECT new.id, value FROM json_each(new.files_modified);
				END;

				-- Backfill every row that predates the trigger. INSERT OR
				-- IGNORE makes re-running harmless, which matters because
				-- migrations are required to be idempotent here (the
				-- SessionStart race retries the whole sequence).
				INSERT OR IGNORE INTO observation_files(observation_id, path)
					SELECT o.id, j.value FROM observations o, json_each(o.files_read) j
					UNION
					SELECT o.id, j.value FROM observations o, json_each(o.files_modified) j;
			`)
			return err
		},
	},
	{
		// next_steps carries what a session left UNFINISHED. Real
		// claude-mem keeps a separate session_summaries table whose
		// columns include it; this port folds summaries into observations,
		// and every other column of that table already had an equivalent
		// here — this was the one that did not.
		//
		// SQLite has no IF NOT EXISTS for ADD COLUMN, and migrations here
		// must be idempotent because the SessionStart race can retry the
		// whole sequence. The pragma check below is how that is achieved
		// without depending on parsing an error string.
		Version: 7,
		Name:    "next_steps column for session summaries",
		Apply: func(ctx context.Context, db *sql.DB) error {
			var n int
			if err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM pragma_table_info('observations') WHERE name = 'next_steps'`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
			_, err := db.ExecContext(ctx,
				`ALTER TABLE observations ADD COLUMN next_steps TEXT NOT NULL DEFAULT '[]'`)
			return err
		},
	},
}

func runMigrations(db *sql.DB) error {
	return migrate.Run(context.Background(), db, migrate.SQLitePlaceholder, migrations)
}
