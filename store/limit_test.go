package store

import "testing"

// TestNegativeLimitDoesNotReturnUnlimitedRows is the regression test for a
// real bug found against this project's own SQLite driver, reproduced
// directly (not assumed from Timeline's identical fix alone): SQLite's
// LIMIT treats a negative value as "unlimited," not "zero." Every
// LIMIT-bounded read path in this file returned every single row in the
// table when given limit=-1, before clampNegativeLimit existed. Seeds 20
// rows and confirms each method returns 0, not 20, for limit=-1.
func TestNegativeLimitDoesNotReturnUnlimitedRows(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	const n = 20
	for i := 0; i < n; i++ {
		title := "row"
		if _, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", title, string(rune('a'+i))), Observation{Type: "discovery", Title: title}, 0); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}

	t.Run("Search", func(t *testing.T) {
		results, err := st.Search("proj", "row", "", -1, 0)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("Search(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("RecentByProject", func(t *testing.T) {
		results, err := st.RecentByProject("proj", -1)
		if err != nil {
			t.Fatalf("RecentByProject: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("RecentByProject(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("BySessionID", func(t *testing.T) {
		results, err := st.BySessionID("s1", -1)
		if err != nil {
			t.Fatalf("BySessionID: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("BySessionID(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("ExportAll", func(t *testing.T) {
		results, err := st.ExportAll(0, -1)
		if err != nil {
			t.Fatalf("ExportAll: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("ExportAll(limit=-1) returned %d rows, want 0", len(results))
		}
	})
	t.Run("ObservationsNeedingEmbedding", func(t *testing.T) {
		results, err := st.ObservationsNeedingEmbedding("proj", 768, 0, -1)
		if err != nil {
			t.Fatalf("ObservationsNeedingEmbedding: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("ObservationsNeedingEmbedding(limit=-1) returned %d rows, want 0", len(results))
		}
	})
}

// TestSemanticSearchNegativeLimitDoesNotPanic is the regression test for
// the more severe version of the same bug: SemanticSearch slices its
// results in Go (all[:limit]) rather than relying on SQL's LIMIT, so a
// negative limit didn't return "everything" — it panicked outright with
// "slice bounds out of range," confirmed against a real seeded database
// before this fix. Since this runs inside the worker daemon's per-event
// goroutine and the MCP server's request handler, an unrecovered panic
// here would have crashed the entire shared process.
func TestSemanticSearchNegativeLimitDoesNotPanic(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	res, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"), Observation{Type: "discovery", Title: "x"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(res.ID, make([]float32, 768)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	matches, err := st.SemanticSearch("proj", make([]float32, 768), -1)
	if err != nil {
		t.Fatalf("SemanticSearch(limit=-1): %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("SemanticSearch(limit=-1) returned %d matches, want 0", len(matches))
	}
}
