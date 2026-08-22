package postgres

import (
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// backdateCreatedAtEpoch is the Postgres equivalent of the SQLite tests'
// setCreatedAtEpoch — Insert always stamps "now," so a real cutoff test
// needs a row that's actually old.
func backdateCreatedAtEpoch(t *testing.T, s *Store, id int64, epoch int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE observations SET created_at_epoch = $1 WHERE id = $2`, epoch, id); err != nil {
		t.Fatalf("backdate observation %d: %v", id, err)
	}
}

func TestPostgresPruneDryRunCountsWithoutDeleting(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	old, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "a", project),
		memory.Observation{Type: "discovery", Title: "old row"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	backdateCreatedAtEpoch(t, st, old.ID, 1000)

	n, err := st.Prune(project, 2000, true)
	if err != nil {
		t.Fatalf("Prune (dry run): %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune dry-run count = %d, want 1", n)
	}
	count, err := st.CountByProject(project)
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject after a dry run = %d, want 1 (dry run must not delete anything)", count)
	}
}

func TestPostgresPruneDeletesOnlyRowsOlderThanCutoffScopedToProject(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	otherProject := uniqueProject(t)

	old, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "old", project),
		memory.Observation{Type: "discovery", Title: "old row"}, 0)
	if err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	backdateCreatedAtEpoch(t, st, old.ID, 1000)

	recent, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "recent", project),
		memory.Observation{Type: "discovery", Title: "recent row"}, 0)
	if err != nil {
		t.Fatalf("Insert recent: %v", err)
	}
	backdateCreatedAtEpoch(t, st, recent.ID, 5000)

	oldOther, err := st.Insert("s1", otherProject, "Bash", memory.ContentHash("s1", "Bash", "old-other", otherProject),
		memory.Observation{Type: "discovery", Title: "old row in another project"}, 0)
	if err != nil {
		t.Fatalf("Insert old (other project): %v", err)
	}
	backdateCreatedAtEpoch(t, st, oldOther.ID, 1000)

	n, err := st.Prune(project, 2000, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune scoped to project deleted %d rows, want 1", n)
	}

	count, err := st.CountByProject(project)
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject(project) after prune = %d, want 1 (only the recent row left)", count)
	}
	otherCount, err := st.CountByProject(otherProject)
	if err != nil {
		t.Fatalf("CountByProject(otherProject): %v", err)
	}
	if otherCount != 1 {
		t.Fatalf("CountByProject(otherProject) after a prune scoped to a different project = %d, want 1 (untouched)", otherCount)
	}
}

// TestPostgresPruneCleansUpSearchAndEmbeddingColumnsToo confirms a pruned
// row is genuinely gone from both the keyword index (the generated
// search_vector column, part of the same row) and semantic search (the
// embedding column, also part of the same row) — unlike SQLite there's no
// separate shadow table to go stale, but this is still worth a real
// end-to-end check against the live container rather than assuming a
// plain DELETE is enough.
func TestPostgresPruneCleansUpSearchAndEmbeddingColumnsToo(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	old, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "a", project),
		memory.Observation{Type: "discovery", Title: "prunable marker xyzzy-plumbus"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	backdateCreatedAtEpoch(t, st, old.ID, 1000)

	dims := make([]float32, DefaultEmbedDims)
	dims[0] = 1
	if err := st.SaveEmbedding(old.ID, dims); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	before, err := st.Search(project, "xyzzy-plumbus", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search before prune: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("Search before prune found %d results, want 1", len(before))
	}

	if _, err := st.Prune(project, 2000, false); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := st.Search(project, "xyzzy-plumbus", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search after prune: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("Search after prune still found %d results", len(after))
	}

	query := make([]float32, DefaultEmbedDims)
	query[0] = 1
	matches, err := st.SemanticSearch(project, query, 10)
	if err != nil {
		t.Fatalf("SemanticSearch after prune: %v", err)
	}
	for _, m := range matches {
		if m.ID == old.ID {
			t.Fatalf("SemanticSearch after prune still returned the pruned row (id=%d)", old.ID)
		}
	}
}

// TestPostgresPruneCutoffUnitsMatchInsertsRealTimestamp is the Postgres
// half of the real cutoff-unit bug regression (see store's
// prune_units_test.go): Insert stamps created_at_epoch with
// time.Now().UnixMilli() here too (postgres.go), so a caller's cutoff
// must be in the same unit — cmd's cmdPrune originally used
// time.Now().AddDate(...).Unix() (seconds), a ~1000x mismatch that made
// prune silently delete nothing, ever. Uses a real Insert-stamped row,
// not a hand-picked epoch value, so a unit mismatch would actually show
// up here the way it wouldn't in the other, unit-agnostic Prune tests.
func TestPostgresPruneCutoffUnitsMatchInsertsRealTimestamp(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	if _, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "a", project),
		memory.Observation{Type: "discovery", Title: "inserted with a real timestamp"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	cutoff := time.Now().AddDate(0, 0, 1).UnixMilli()
	n, err := st.Prune(project, cutoff, true)
	if err != nil {
		t.Fatalf("Prune (dry run): %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune found %d rows older than tomorrow (in milliseconds), want 1 — "+
			"a real Insert-stamped row not being found points at a unit mismatch", n)
	}
}
