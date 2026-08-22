package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func openTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fp.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func countPaths(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow("SELECT count(*) FROM observation_files").Scan(&n); err != nil {
		t.Fatalf("count observation_files: %v", err)
	}
	return n
}

// TestObservationFilesTriggerPopulatesOnInsert covers the mechanism the
// whole index depends on. Sync is done by trigger rather than from Go
// precisely so it holds for every write path — Insert, ImportRow, and
// anything added later — so the trigger firing is the thing worth
// pinning, not any one caller.
func TestObservationFilesTriggerPopulatesOnInsert(t *testing.T) {
	st, _ := openTempStore(t)

	if _, err := st.Insert(context.Background(), "s1", "p", "Read", memory.ContentHash("s1", "Read", "a", "1"),
		memory.Observation{Type: "change", Title: "a",
			FilesRead:     []string{"/x.go", "/y.go"},
			FilesModified: []string{"/y.go", "/z.go"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// /y.go is in BOTH lists; the union must not double-count it, or the
	// join would return the observation twice and the old DISTINCT would
	// still be load-bearing.
	if got := countPaths(t, st); got != 3 {
		t.Fatalf("indexed %d paths, want 3 (/x.go, /y.go, /z.go — y deduped across both lists)", got)
	}

	got, err := st.ObservationsForFile(context.Background(), "p", "/y.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ObservationsForFile(/y.go) returned %d rows, want exactly 1 — a path in both "+
			"files_read and files_modified must not yield a duplicate", len(got))
	}
}

// TestObservationFilesIgnoresADuplicateInsert guards the dedup path: an
// Insert whose content_hash already exists writes no row, so the trigger
// must not fire and inflate the index either.
func TestObservationFilesIgnoresADuplicateInsert(t *testing.T) {
	st, _ := openTempStore(t)
	obs := memory.Observation{Type: "change", Title: "a", FilesRead: []string{"/x.go"}}
	hash := memory.ContentHash("s1", "Read", "a", "1")

	for i := 0; i < 3; i++ {
		if _, err := st.Insert(context.Background(), "s1", "p", "Read", hash, obs, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	if got := countPaths(t, st); got != 1 {
		t.Fatalf("indexed %d paths after three identical inserts, want 1", got)
	}
}

// TestObservationFilesCascadesOnDelete pins that prune does not leave the
// index behind. This works only because the DSN sets _foreign_keys=on —
// without it SQLite parses ON DELETE CASCADE and silently ignores it,
// which is exactly the kind of thing that looks fine until the index
// quietly disagrees with the table.
func TestObservationFilesCascadesOnDelete(t *testing.T) {
	st, _ := openTempStore(t)
	res, err := st.Insert(context.Background(), "s1", "p", "Read", memory.ContentHash("s1", "Read", "a", "1"),
		memory.Observation{Type: "change", Title: "a", FilesRead: []string{"/x.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if countPaths(t, st) != 1 {
		t.Fatal("path not indexed on insert")
	}

	if _, err := st.db.Exec("DELETE FROM observations WHERE id = ?", res.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := countPaths(t, st); got != 0 {
		t.Fatalf("%d orphaned path rows after deleting the observation — ON DELETE CASCADE did "+
			"not fire, so the index now claims files for a row that no longer exists", got)
	}
}

// TestObservationFilesBackfillsExistingRows covers the upgrade path. A
// store that predates this index must not silently return nothing for
// every file — which is precisely what a missing backfill would do, since
// the read now joins the index rather than scanning.
func TestObservationFilesBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a store, then simulate "created before migration 6" by
	// dropping the table and trigger and rewinding the version.
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 20; i++ {
		title := fmt.Sprintf("row %d", i)
		if _, err := st.Insert(context.Background(), "s1", "p", "Read", memory.ContentHash("s1", "Read", title, "x"),
			memory.Observation{Type: "change", Title: title,
				FilesRead: []string{fmt.Sprintf("/f%d.go", i%4)}}, 0); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if _, err := st.db.Exec(`
		DROP TRIGGER IF EXISTS observation_files_ai;
		DROP TABLE IF EXISTS observation_files;
		DELETE FROM schema_migrations WHERE version = 6;`); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	st.Close()

	// Reopening must re-run migration 6 and backfill.
	st2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	// One row per (observation, path) pair: 20 observations x 1 path each.
	// Not 4 — that is the distinct-path count, asserted separately below.
	if got := countPaths(t, st2); got != 20 {
		t.Fatalf("backfill produced %d index rows, want 20 (one per observation/path pair)", got)
	}
	var distinct int
	if err := st2.db.QueryRow("SELECT count(DISTINCT path) FROM observation_files").Scan(&distinct); err != nil {
		t.Fatalf("count distinct: %v", err)
	}
	if distinct != 4 {
		t.Fatalf("backfill indexed %d distinct paths, want 4", distinct)
	}
	found, err := st2.ObservationsForFile(context.Background(), "p", "/f2.go", 50)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(found) != 5 {
		t.Fatalf("found %d observations for a backfilled path, want 5 — a store created before "+
			"this migration would otherwise return nothing for every file it knows about", len(found))
	}
}

// TestObservationFilesBackfillIsIdempotent matters because migrations here
// are required to be re-runnable: the SessionStart race retries the whole
// sequence, so a backfill that duplicated rows on a second run would
// corrupt the index rather than merely waste time.
func TestObservationFilesBackfillIsIdempotent(t *testing.T) {
	st, path := openTempStore(t)
	if _, err := st.Insert(context.Background(), "s1", "p", "Read", memory.ContentHash("s1", "Read", "a", "1"),
		memory.Observation{Type: "change", Title: "a", FilesRead: []string{"/x.go", "/y.go"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	before := countPaths(t, st)

	for i := 0; i < 3; i++ {
		if _, err := st.db.Exec("DELETE FROM schema_migrations WHERE version = 6"); err != nil {
			t.Fatalf("rewind: %v", err)
		}
		if err := runMigrations(context.Background(), st.db); err != nil {
			t.Fatalf("re-run migrations: %v", err)
		}
	}
	if got := countPaths(t, st); got != before {
		t.Fatalf("re-running the backfill changed the index from %d to %d rows", before, got)
	}
	_ = path
	var _ *sql.DB = st.db
}

// TestImportRebuildsTheFilePathIndex covers the documented SQLite<->Postgres
// migration path against the file index added in migration 6.
//
// The index is populated by a trigger on INSERT rather than from Go, and
// ImportRow goes through that same insert — so this is really asking
// whether the trigger holds for a write path nobody wrote it for. If it
// did not, a migrated store would look complete (observations and
// embeddings all present) while the PreToolUse file-context lookup
// silently found nothing on every file, which is precisely the failure
// mode that took a full-stack soak to notice the first time.
func TestImportRebuildsTheFilePathIndex(t *testing.T) {
	src, srcPath := openTempStore(t)
	target := "/repo/src/auth/tokens.go"
	if _, err := src.Insert(context.Background(), "s1", "repo", "Read", memory.ContentHash("s1", "Read", "a", "1"),
		memory.Observation{Type: "discovery", Title: "read tokens", FilesRead: []string{target}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	rows, err := src.ExportAll(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("exported %d rows, want 1", len(rows))
	}
	_ = srcPath

	dst, _ := openTempStore(t)
	if _, err := dst.ImportRow(context.Background(), rows[0]); err != nil {
		t.Fatalf("ImportRow: %v", err)
	}

	if got := countPaths(t, dst); got != 1 {
		t.Fatalf("the migrated store has %d indexed paths, want 1 — the trigger did not fire on "+
			"the import path, so file-context would find nothing for every file", got)
	}
	found, err := dst.ObservationsForFile(context.Background(), "repo", target, 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("looking up %q on the migrated store found %d, want 1", target, len(found))
	}

	// Re-importing is documented as safe; it must not duplicate index rows
	// either, which a trigger firing on a no-op insert could have caused.
	if _, err := dst.ImportRow(context.Background(), rows[0]); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if got := countPaths(t, dst); got != 1 {
		t.Fatalf("re-import inflated the index to %d rows, want 1", got)
	}
}
