package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// TestRunAppliesInVersionOrderRegardlessOfSliceOrder is the regression
// test for a real bug found by hand: "ascending Version order" was, until
// this fix, only ever true if the caller happened to list migrations in
// that order in the slice literal — Run() itself never sorted them. A
// migrations slice with version 2 listed before version 1 applied version
// 2 FIRST, silently violating the one guarantee this whole package exists
// to provide (every real caller today happens to list migrations in
// order, which is exactly why this went unnoticed).
func TestRunAppliesInVersionOrderRegardlessOfSliceOrder(t *testing.T) {
	db := openTestDB(t)
	var order []int
	migrations := []Migration{
		{Version: 3, Name: "third", Apply: func(ctx context.Context, db *sql.DB) error {
			order = append(order, 3)
			return nil
		}},
		{Version: 1, Name: "first", Apply: func(ctx context.Context, db *sql.DB) error {
			order = append(order, 1)
			return nil
		}},
		{Version: 2, Name: "second", Apply: func(ctx context.Context, db *sql.DB) error {
			order = append(order, 2)
			return nil
		}},
	}
	if err := Run(context.Background(), db, SQLitePlaceholder, migrations); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []int{1, 2, 3}
	if len(order) != len(want) {
		t.Fatalf("applied order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("applied order = %v, want %v (ascending version, not slice order)", order, want)
		}
	}
}

// TestRunRejectsDuplicateVersions is the regression test for a more
// serious silent failure the same missing check allowed: before this fix,
// two migrations accidentally sharing a Version number didn't error —
// once the first one got recorded as applied, the "already applied?"
// check silently skipped the second one forever, with nothing
// distinguishing that from having run correctly. Confirmed directly: the
// second migration's Apply was never even called.
func TestRunRejectsDuplicateVersions(t *testing.T) {
	db := openTestDB(t)
	var ran []string
	migrations := []Migration{
		{Version: 1, Name: "first-a", Apply: func(ctx context.Context, db *sql.DB) error {
			ran = append(ran, "first-a")
			return nil
		}},
		{Version: 1, Name: "first-b-a-different-migration", Apply: func(ctx context.Context, db *sql.DB) error {
			ran = append(ran, "first-b-a-different-migration")
			return nil
		}},
	}
	err := Run(context.Background(), db, SQLitePlaceholder, migrations)
	if err == nil {
		t.Fatal("Run with two migrations sharing version 1: want an error, got nil")
	}
	if len(ran) != 0 {
		t.Fatalf("Run partially executed duplicate-version migrations before erroring: %v — want it to fail the up-front check before applying anything", ran)
	}
}

// TestRunSurvivesConcurrentCallersOnAFreshDatabase is the regression test
// for a real race found by hand against this project's own actual
// architecture: multiple independent processes (the worker daemon spawned
// by SessionStart's `start`, and any of `context`/`file-context`/other
// hook subcommands) each call backend.Open — and therefore migrate.Run —
// on the SAME SQLite file, most likely to collide on a brand-new
// database's very first session.
//
// Reproduced directly before this fix existed: several independent *sql.DB
// connections to the same fresh file, run concurrently, surfaced three
// DIFFERENT real errors depending on timing — "database is locked" (a
// losing DDL statement, which busy_timeout doesn't cover the way it
// covers a losing row lock), "UNIQUE constraint failed:
// schema_migrations.version" (two connections both saw a migration as
// unapplied and both tried to record it), and "duplicate column name" (a
// migration's own idempotency check racing against an identical
// concurrent check — see store.ensureContentHashColumn). Retrying the
// whole Run() body (rather than trying to prevent the race at the SQL
// level) fixes all three at once, since every Migration.Apply is already
// required to be idempotent.
//
// Uses independent *sql.DB handles to the same file path, not goroutines
// sharing one *sql.DB — a single connection pool can mask this race in
// ways a real multi-process scenario never would.
func TestRunSurvivesConcurrentCallersOnAFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	migrations := []Migration{
		{Version: 1, Name: "table one", Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`)
			return err
		}},
		{Version: 2, Name: "add column", Apply: func(ctx context.Context, db *sql.DB) error {
			rows, err := db.QueryContext(ctx, `PRAGMA table_info(t1)`)
			if err != nil {
				return err
			}
			hasCol := false
			for rows.Next() {
				var cid int
				var name, ctype string
				var notnull, pk int
				var dflt sql.NullString
				if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
					rows.Close()
					return err
				}
				if name == "extra" {
					hasCol = true
				}
			}
			rows.Close()
			if hasCol {
				return nil
			}
			// Widens the exact check-then-ALTER window that produced a
			// real "duplicate column name" error in production (see
			// store.ensureContentHashColumn) — without this, whether
			// concurrent goroutines actually land inside this race
			// depends on scheduler luck, and a flaky-but-usually-passing
			// test would be worse than no test at all.
			time.Sleep(5 * time.Millisecond)
			_, err = db.ExecContext(ctx, `ALTER TABLE t1 ADD COLUMN extra TEXT`)
			return err
		}},
	}

	const concurrency = 8
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("sql.Open %d: %v", i, err)
		}
		t.Cleanup(func() { db.Close() })
		wg.Add(1)
		go func(i int, db *sql.DB) {
			defer wg.Done()
			<-start // released together, to maximize real contention
			errs[i] = Run(context.Background(), db, SQLitePlaceholder, migrations)
		}(i, db)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Run %d: %v", i, err)
		}
	}
}
