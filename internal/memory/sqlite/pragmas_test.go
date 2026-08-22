package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestOpenEnablesWALAndForeignKeys locks in the two pragmas Open sets via
// DSN params (see sqliteDSNParams's doc comment) — both defaults SQLite
// itself would otherwise leave off.
func TestOpenEnablesWALAndForeignKeys(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var journalMode string
	if err := st.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var fk int
	if err := st.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (on)", fk)
	}
}

// TestOpenBoundsThePool locks in that Open caps the pool (see
// sqliteMaxOpenConns) rather than leaving database/sql's unlimited
// default, the way postgres.Open already did.
func TestOpenBoundsThePool(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if got := st.db.Stats().MaxOpenConnections; got != sqliteMaxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, sqliteMaxOpenConns)
	}
}

// TestConcurrentStoresCanBothWriteWithoutLockErrors is the real regression
// this exists for: this project's actual deployment shape is the worker
// daemon and every CLI subcommand (search, doctor, context, stop,
// file-context, prune, the MCP server) each opening their OWN *Store
// against the same file. Reproduced directly before the WAL/busy_timeout
// fix: one Store holding an open write transaction made a second Store's
// concurrent Insert fail immediately with "database is locked"
// (SQLITE_BUSY). WAL mode plus a busy_timeout means the second writer
// waits briefly and succeeds instead.
func TestConcurrentStoresCanBothWriteWithoutLockErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	a, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	defer a.Close()
	b, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		st := a
		if i%2 == 0 {
			st = b
		}
		go func(st *Store, i int) {
			defer wg.Done()
			_, err := st.Insert(context.Background(), "s1", "proj", "Bash",
				memory.ContentHash("s1", "Bash", string(rune('a'+i%26)), string(rune(i))),
				memory.Observation{Type: "discovery", Title: "concurrent write"}, 0)
			errs <- err
		}(st, i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Insert from one of two separate *Store handles on the same file failed: %v", err)
		}
	}

	count, err := a.CountByProject(context.Background(), "proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != n {
		t.Fatalf("CountByProject = %d, want %d (every concurrent write should have landed)", count, n)
	}
}
