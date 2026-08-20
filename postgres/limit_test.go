package postgres

import (
	"testing"

	"claude-mem-go/store"
)

// TestNegativeLimitDoesNotError is the Postgres-side regression test for
// the same real bug found against SQLite (see store's identically-named
// test): every LIMIT-bounded query here failed outright with a real
// "LIMIT must not be negative" driver error when given limit=-1, before
// clampNegativeLimit existed — a different failure mode from SQLite's
// silent "unlimited," but still the wrong outcome for callers.
func TestNegativeLimitDoesNotError(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	res, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project), store.Observation{Type: "discovery", Title: "row"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(res.ID, make([]float32, DefaultEmbedDims)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	t.Run("Search", func(t *testing.T) {
		results, err := st.Search(project, "row", -1)
		if err != nil {
			t.Fatalf("Search(limit=-1): want a clean clamp, got an error: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("Search(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("RecentByProject", func(t *testing.T) {
		results, err := st.RecentByProject(project, -1)
		if err != nil {
			t.Fatalf("RecentByProject(limit=-1): want a clean clamp, got an error: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("RecentByProject(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("BySessionID", func(t *testing.T) {
		results, err := st.BySessionID("s1", -1)
		if err != nil {
			t.Fatalf("BySessionID(limit=-1): want a clean clamp, got an error: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("BySessionID(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("SemanticSearch", func(t *testing.T) {
		matches, err := st.SemanticSearch(project, make([]float32, DefaultEmbedDims), -1)
		if err != nil {
			t.Fatalf("SemanticSearch(limit=-1): want a clean clamp, got an error: %v", err)
		}
		if len(matches) != 0 {
			t.Errorf("SemanticSearch(limit=-1) returned %d matches, want 0", len(matches))
		}
	})
	t.Run("ExportAll", func(t *testing.T) {
		results, err := st.ExportAll(0, -1)
		if err != nil {
			t.Fatalf("ExportAll(limit=-1): want a clean clamp, got an error: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("ExportAll(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("ObservationsNeedingEmbedding", func(t *testing.T) {
		results, err := st.ObservationsNeedingEmbedding(project, DefaultEmbedDims, 0, -1)
		if err != nil {
			t.Fatalf("ObservationsNeedingEmbedding(limit=-1): want a clean clamp, got an error: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("ObservationsNeedingEmbedding(limit=-1) returned %d rows, want 0", len(results))
		}
	})
}
