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
}

func runMigrations(db *sql.DB) error {
	return migrate.Run(context.Background(), db, migrate.SQLitePlaceholder, migrations)
}
