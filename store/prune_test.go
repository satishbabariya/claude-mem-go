package store

import (
	"path/filepath"
	"testing"
)

func openPruneTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// setCreatedAtEpoch backdates a row directly — Insert always stamps "now,"
// and this package's tests don't have a real historical database to prune
// against, so this is the same technique dedup_test.go's pre-migration
// simulation uses: manipulate the schema directly to construct the state
// under test.
func setCreatedAtEpoch(t *testing.T, s *Store, id int64, epoch int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE observations SET created_at_epoch = ? WHERE id = ?`, epoch, id); err != nil {
		t.Fatalf("backdate observation %d: %v", id, err)
	}
}

func TestPruneDryRunCountsWithoutDeleting(t *testing.T) {
	st := openPruneTestStore(t)
	old, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"),
		Observation{Type: "discovery", Title: "old row"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	setCreatedAtEpoch(t, st, old.ID, 1000)

	n, err := st.Prune("", 2000, true)
	if err != nil {
		t.Fatalf("Prune (dry run): %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune dry-run count = %d, want 1", n)
	}

	count, err := st.CountByProject("proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject after a dry run = %d, want 1 (dry run must not delete anything)", count)
	}
}

func TestPruneDeletesOnlyRowsOlderThanCutoff(t *testing.T) {
	st := openPruneTestStore(t)
	old, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "old", "1"),
		Observation{Type: "discovery", Title: "old row"}, 0)
	if err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	setCreatedAtEpoch(t, st, old.ID, 1000)

	recent, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "recent", "2"),
		Observation{Type: "discovery", Title: "recent row"}, 0)
	if err != nil {
		t.Fatalf("Insert recent: %v", err)
	}
	setCreatedAtEpoch(t, st, recent.ID, 5000)

	n, err := st.Prune("", 2000, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune deleted %d rows, want 1", n)
	}

	results, err := st.RecentByProject("proj", 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(results) != 1 || results[0].ID != recent.ID {
		t.Fatalf("after Prune, results = %v, want only the recent row (%d)", results, recent.ID)
	}
}

func TestPruneScopesToProjectWhenGiven(t *testing.T) {
	st := openPruneTestStore(t)
	a, err := st.Insert("s1", "proj-a", "Bash", ContentHash("s1", "Bash", "a", "1"),
		Observation{Type: "discovery", Title: "old in proj-a"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	setCreatedAtEpoch(t, st, a.ID, 1000)

	b, err := st.Insert("s1", "proj-b", "Bash", ContentHash("s1", "Bash", "b", "2"),
		Observation{Type: "discovery", Title: "old in proj-b"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	setCreatedAtEpoch(t, st, b.ID, 1000)

	n, err := st.Prune("proj-a", 2000, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune scoped to proj-a deleted %d rows, want 1", n)
	}

	countA, err := st.CountByProject("proj-a")
	if err != nil {
		t.Fatalf("CountByProject proj-a: %v", err)
	}
	if countA != 0 {
		t.Fatalf("CountByProject(proj-a) after scoped prune = %d, want 0", countA)
	}
	countB, err := st.CountByProject("proj-b")
	if err != nil {
		t.Fatalf("CountByProject proj-b: %v", err)
	}
	if countB != 1 {
		t.Fatalf("CountByProject(proj-b) after prune scoped to proj-a = %d, want 1 (untouched)", countB)
	}
}

// TestMigration5FixesAnAlreadyMigratedDatabasesBrokenTrigger reproduces
// the exact real-world upgrade scenario: a database that already ran
// migration 3 (and so has migration 3 recorded as applied) has the
// original broken observations_ad trigger — createFTSSQL's own `CREATE
// TRIGGER IF NOT EXISTS` is a no-op against an existing trigger, so only
// an explicit migration reaches it. Simulate that state by hand (drop the
// fixed trigger, recreate the original broken one) and confirm reopening
// applies migration 5 and Prune then works.
func TestMigration5FixesAnAlreadyMigratedDatabasesBrokenTrigger(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.db.Exec(`
		DROP TRIGGER IF EXISTS observations_ad;
		CREATE TRIGGER observations_ad AFTER DELETE ON observations BEGIN
			INSERT INTO observations_fts(observations_fts, rowid, title, subtitle, narrative, facts, concepts)
			VALUES ('delete', old.id, old.title, old.subtitle, old.narrative, old.facts, old.concepts);
		END;
	`); err != nil {
		t.Fatalf("install the original broken trigger: %v", err)
	}
	// Also roll back the migration record for it, exactly as a database
	// that genuinely only ever ran the old migration 3 would look —
	// otherwise Run would (correctly) skip re-applying a version it
	// believes it already has.
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE version = 5`); err != nil {
		t.Fatalf("roll back migration 5's record: %v", err)
	}
	old, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"),
		Observation{Type: "discovery", Title: "row inserted under the broken trigger"}, 0)
	if err != nil {
		t.Fatalf("Insert under simulated broken trigger: %v", err)
	}
	setCreatedAtEpoch(t, st, old.ID, 1000)
	st.Close()

	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-Open (should apply migration 5): %v", err)
	}
	defer st2.Close()

	if _, err := st2.Prune("", 2000, false); err != nil {
		t.Fatalf("Prune after migration 5 should succeed against the fixed trigger, got: %v", err)
	}
}

// TestPruneCleansUpFTSIndexToo is the real regression this exists to
// guard: a naive DELETE that only touched the observations table but left
// the FTS5 shadow table stale would make a pruned row's content keep
// matching Search forever. observations_ad (search.go) is supposed to
// prevent that.
func TestPruneCleansUpFTSIndexToo(t *testing.T) {
	st := openPruneTestStore(t)
	old, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"),
		Observation{Type: "discovery", Title: "prunable marker xyzzy-plumbus"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	setCreatedAtEpoch(t, st, old.ID, 1000)

	before, err := st.Search("", "xyzzy-plumbus", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search before prune: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("Search before prune found %d results, want 1", len(before))
	}

	if _, err := st.Prune("", 2000, false); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := st.Search("", "xyzzy-plumbus", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search after prune: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("Search after prune still found %d results — the FTS5 index was not cleaned up", len(after))
	}
}
