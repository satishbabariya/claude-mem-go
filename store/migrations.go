package store

import (
	"context"
	"database/sql"

	"claude-mem-go/migrate"
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
}

func runMigrations(db *sql.DB) error {
	return migrate.Run(context.Background(), db, migrate.SQLitePlaceholder, migrations)
}
