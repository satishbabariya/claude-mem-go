package sqlite

import (
	"path/filepath"
	"testing"
)

// TestOpenRecordsAllMigrationsOnAFreshDatabase locks in the versioning
// path itself, not just each migration's individual effect (covered
// elsewhere) — a fresh Open must leave schema_migrations with exactly the
// versions this package currently declares, in order, so a later added
// migration has an accurate "already applied?" baseline to check against.
func TestOpenRecordsAllMigrationsOnAFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rows, err := st.db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	var gotVersions []int
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotVersions = append(gotVersions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(gotVersions) != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, want %d (one per declared migration): %v", len(gotVersions), len(migrations), gotVersions)
	}
	for i, m := range migrations {
		if gotVersions[i] != m.Version {
			t.Fatalf("schema_migrations[%d] = version %d, want %d", i, gotVersions[i], m.Version)
		}
	}
}

// TestReopenDoesNotReapplyMigrations confirms opening an already-migrated
// database a second time is a fast no-op for every migration, not just
// individually idempotent SQL — Open must not re-run Apply funcs at all.
func TestReopenDoesNotReapplyMigrations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	st.Close()

	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer st2.Close()

	var count int
	if err := st2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("schema_migrations has %d rows after reopening, want %d (no duplicate rows from re-applying)", count, len(migrations))
	}
}
