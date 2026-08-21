// Package migrate is a minimal, real schema-versioning framework shared by
// both storage backends (store's SQLite and postgres's Postgres+pgvector).
//
// Neither backend had one before this: SQLite grew its one real schema
// change (the content_hash column, see store/dedup.go) as a bespoke
// "check PRAGMA table_info, ALTER if missing" function with no record of
// what had been applied; Postgres's schema was a single idempotent
// CREATE-IF-NOT-EXISTS block with no ALTER path at all. Both work for the
// schema each backend has today, but neither has anywhere to put the NEXT
// schema change without either repeating that ad hoc pattern again or
// editing a CREATE TABLE that already-populated production databases have
// long since run once and will never re-run.
//
// This package doesn't replace those existing idempotent statements — it
// wraps them: each one becomes a numbered Migration, applied at most once
// per database and recorded in a schema_migrations table. Every Migration's
// Apply must stay idempotent on its own (CREATE ... IF NOT EXISTS, or a
// column-existence check before ALTER) — that's what lets Run() be called
// safely against a database that already has the schema from before this
// package existed: applying an already-satisfied migration is a no-op, and
// it still gets recorded so the next migration's "already applied?" check
// is accurate.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Migration is one numbered, idempotent schema change.
type Migration struct {
	Version int
	Name    string
	Apply   func(ctx context.Context, db *sql.DB) error
}

const createTrackingTableSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TEXT NOT NULL
)`

// Placeholder builds one dialect's positional-parameter syntax. The rest of
// this package's SQL (the tracking table, the SELECT) is written to be
// valid in both SQLite and Postgres already; the three-value INSERT is the
// only place that isn't, since SQLite wants "?" and Postgres wants "$1".
type Placeholder func(argIndex int) string

func SQLitePlaceholder(argIndex int) string   { return "?" }
func PostgresPlaceholder(argIndex int) string { return fmt.Sprintf("$%d", argIndex) }

// Run applies every migration in migrations, in ascending Version order,
// that isn't already recorded in schema_migrations — recording each one as
// it succeeds, so a failure partway through leaves already-applied
// migrations correctly marked and retries pick up where it stopped rather
// than re-running everything.
//
// "Ascending Version order" was, until found by hand, only ever true if
// the caller happened to list migrations in that order in the slice
// literal — nothing here actually sorted them. A migrations slice with
// version 2 listed before version 1 applied version 2 FIRST, silently
// violating the one guarantee this whole package exists to provide.
// Sorted explicitly now, independent of slice order.
//
// A duplicate Version number was an even more serious silent failure:
// once the first migration with that version got recorded as applied,
// the second one sharing the same number was skipped by the "already
// applied?" check — with no error, indistinguishable from having run
// correctly. Rejected outright now rather than silently dropping one.
func Run(ctx context.Context, db *sql.DB, ph Placeholder, migrations []Migration) error {
	sorted := make([]Migration, len(migrations))
	copy(sorted, migrations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	seen := map[int]string{}
	for _, m := range sorted {
		if prior, ok := seen[m.Version]; ok {
			return fmt.Errorf("duplicate migration version %d: %q and %q both claim it", m.Version, prior, m.Name)
		}
		seen[m.Version] = m.Name
	}
	migrations = sorted

	if _, err := db.ExecContext(ctx, createTrackingTableSQL); err != nil {
		return fmt.Errorf("create schema_migrations tracking table: %w", err)
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied migration version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	insertSQL := fmt.Sprintf(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (%s, %s, %s)`,
		ph(1), ph(2), ph(3))

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := m.Apply(ctx, db); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
		if _, err := db.ExecContext(ctx, insertSQL, m.Version, m.Name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("record migration %d (%s) as applied: %w", m.Version, m.Name, err)
		}
		applied[m.Version] = true
	}
	return nil
}
