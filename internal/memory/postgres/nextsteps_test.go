package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestNextStepsRoundTripPostgres is the Postgres half of the SQLite test
// of the same name. Both are needed rather than one standing in for the
// other: the column is JSONB here and TEXT there, the two INSERTs are
// written out separately with their own placeholder lists, and the
// migrations are entirely different statements.
//
// Everything is scoped to a uniqueProject and a hash derived from it,
// following this package's existing convention. That convention exists
// because this Postgres instance is long-lived and shared, unlike the
// SQLite tests' fresh temp file: a fixed content_hash makes every run
// after the first hit ON CONFLICT DO NOTHING, so the test silently reads
// the FIRST run's row and reports whatever that happened to contain.
func TestNextStepsRoundTripPostgres(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	hash := memory.ContentHash(project, "SessionSummary", "np-summary", "")

	want := []string{"finish the pgvector migration", "re-run the scoped benchmark"}
	res, err := st.Insert(context.Background(), "s1", project, "SessionSummary", hash,
		memory.Observation{Type: "summary", Title: "session summary", NextSteps: want}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	recent, err := st.RecentByProject(context.Background(), project, 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("RecentByProject returned %d rows, want 1", len(recent))
	}
	if got := recent[0].Observation.NextSteps; !equalStrs(got, want) {
		t.Errorf("NextSteps from RecentByProject = %v, want %v", got, want)
	}

	// Export from just before this row's own id. ExportAll pages by id
	// ascending, so asking from 0 on a shared database with thousands of
	// prior rows would never reach this one.
	rows, err := st.ExportAll(context.Background(), res.ID-1, 5)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ContentHash != hash {
			continue
		}
		found = true
		// Export is where this field would be silently dropped: the value
		// is scanned into a local either way, so a missing assignment
		// compiles cleanly and loses the data only on a real migration.
		if got := r.Observation.NextSteps; !equalStrs(got, want) {
			t.Errorf("NextSteps from ExportAll = %v, want %v — captured but lost on backend migration", got, want)
		}
	}
	if !found {
		t.Fatalf("the inserted summary (id=%d) never appeared in ExportAll", res.ID)
	}
}

// TestNextStepsMigrationOnPreexistingPostgresStore covers the upgrade
// path for a database that already exists: migration 5 adds the column,
// and rows written before it must survive and read back empty.
//
// This one is genuinely destructive — it drops a column the whole shared
// database uses — so it runs against its own throwaway database rather
// than the shared one. Dropping and re-adding the column resets EVERY
// row's next_steps to the default, which would silently corrupt any other
// test's data that happened to be in flight.
func TestNextStepsMigrationOnPreexistingPostgresStore(t *testing.T) {
	st, dsn := openThrowawayStore(t)
	project := uniqueProject(t)

	if _, err := st.Insert(context.Background(), "s-old", project, "Bash",
		memory.ContentHash(project, "Bash", "np-old-row", ""),
		memory.Observation{Type: "discovery", Title: "written before next_steps existed"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Both the column and its migration record: leaving the record behind
	// would simulate a state no real database reaches, since an
	// already-recorded version is skipped rather than re-applied.
	if _, err := st.db.Exec(`ALTER TABLE observations DROP COLUMN next_steps`); err != nil {
		t.Fatalf("dropping the column to simulate an older database: %v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE version = 5`); err != nil {
		t.Fatalf("removing the migration record: %v", err)
	}
	st.Close()

	reopened := openStoreAt(t, dsn)
	recent, err := reopened.RecentByProject(context.Background(), project, 10)
	if err != nil {
		t.Fatalf("RecentByProject after migration: %v — the column was not restored", err)
	}
	if len(recent) != 1 {
		t.Fatalf("got %d rows after migration, want the pre-existing 1", len(recent))
	}
	if len(recent[0].Observation.NextSteps) != 0 {
		t.Errorf("a row predating the column has NextSteps = %v, want empty", recent[0].Observation.NextSteps)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// openThrowawayStore creates a brand-new database on the same server and
// opens it, returning the store and its DSN. For destructive tests only:
// the shared test database is used by everything else in this package, so
// a test that drops a column must not run against it.
//
// The database is dropped on cleanup. CREATE/DROP DATABASE cannot run
// inside a transaction, and neither can they run while a connection to
// the target is open, which is why the store is closed first.
func openThrowawayStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := requireTestDSN(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing %s: %v", testDSNEnvVar, err)
	}
	name := fmt.Sprintf("cmg_throwaway_%d_%d", time.Now().UnixNano(), uniqueProjectSeq.Add(1))

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("connecting to create a throwaway database: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Skipf("cannot create a throwaway database (%v) — this test needs CREATEDB", err)
	}
	admin.Close()

	u.Path = "/" + name
	dsn := u.String()
	t.Cleanup(func() {
		a, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})
	return openStoreAt(t, dsn), dsn
}

// openStoreAt opens a Store at an explicit DSN, running migrations, and
// closes it on cleanup.
func openStoreAt(t *testing.T, dsn string) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn, DefaultEmbedDims, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", memory.RedactDSN(dsn), err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
