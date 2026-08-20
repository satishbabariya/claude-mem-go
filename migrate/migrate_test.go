package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func TestRunAppliesInOrderAndRecordsEach(t *testing.T) {
	db := openTestDB(t)
	var order []int
	migrations := []Migration{
		{Version: 1, Name: "one", Apply: func(ctx context.Context, db *sql.DB) error {
			order = append(order, 1)
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`)
			return err
		}},
		{Version: 2, Name: "two", Apply: func(ctx context.Context, db *sql.DB) error {
			order = append(order, 2)
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS t2 (id INTEGER PRIMARY KEY)`)
			return err
		}},
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, migrations); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("applied out of order: %v", order)
	}
	if got := appliedVersions(t, db); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("schema_migrations = %v, want [1 2]", got)
	}
}

// TestRunSkipsAlreadyAppliedMigrations is the core correctness property: a
// migration whose version is already recorded must not run its Apply func
// again — real migrations (an ALTER TABLE, an INSERT...SELECT backfill)
// are only safe to run twice because THEY happen to be idempotent, but the
// runner itself must not rely on that; it should skip on its own.
func TestRunSkipsAlreadyAppliedMigrations(t *testing.T) {
	db := openTestDB(t)
	calls := 0
	migrations := []Migration{
		{Version: 1, Name: "one", Apply: func(ctx context.Context, db *sql.DB) error {
			calls++
			return nil
		}},
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, migrations); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, migrations); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Apply called %d times across two Run calls, want 1", calls)
	}
}

// TestRunAppliesOnlyNewMigrationsOnUpgrade simulates the real deployment
// scenario this package exists for: a database that already went through
// migration 1 gets pointed at a newer binary whose migrations list also has
// migration 2. Only migration 2 should run.
func TestRunAppliesOnlyNewMigrationsOnUpgrade(t *testing.T) {
	db := openTestDB(t)
	v1 := []Migration{
		{Version: 1, Name: "one", Apply: func(ctx context.Context, db *sql.DB) error { return nil }},
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, v1); err != nil {
		t.Fatalf("Run v1: %v", err)
	}

	var v2Ran []int
	v2 := []Migration{
		{Version: 1, Name: "one", Apply: func(ctx context.Context, db *sql.DB) error {
			v2Ran = append(v2Ran, 1)
			return nil
		}},
		{Version: 2, Name: "two", Apply: func(ctx context.Context, db *sql.DB) error {
			v2Ran = append(v2Ran, 2)
			return nil
		}},
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, v2); err != nil {
		t.Fatalf("Run v2: %v", err)
	}
	if len(v2Ran) != 1 || v2Ran[0] != 2 {
		t.Fatalf("expected only migration 2 to run on upgrade, got %v", v2Ran)
	}
	if got := appliedVersions(t, db); len(got) != 2 {
		t.Fatalf("schema_migrations = %v, want versions [1 2]", got)
	}
}

// TestRunStopsAtFirstFailureWithoutRecordingIt confirms a failing migration
// is not recorded as applied — a later Run must retry it, not skip it.
func TestRunStopsAtFirstFailureWithoutRecordingIt(t *testing.T) {
	db := openTestDB(t)
	migrations := []Migration{
		{Version: 1, Name: "boom", Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `this is not valid sql`)
			return err
		}},
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, migrations); err == nil {
		t.Fatal("Run with a failing migration returned nil error, want an error")
	}
	if got := appliedVersions(t, db); len(got) != 0 {
		t.Fatalf("a failed migration was recorded as applied: %v", got)
	}
}
