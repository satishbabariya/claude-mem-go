package sqlite

import (
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestObservationsNeedingEmbeddingFindsUnembeddedAndMismatchedRows(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	neverEmbedded, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), memory.Observation{Type: "discovery", Title: "never embedded"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	wrongDims, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "b", "2"), memory.Observation{Type: "discovery", Title: "wrong dims"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(wrongDims.ID, make([]float32, 384)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}
	correctlyEmbedded, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "c", "3"), memory.Observation{Type: "discovery", Title: "already fine"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(correctlyEmbedded.ID, make([]float32, 768)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	results, err := st.ObservationsNeedingEmbedding("proj", 768, 0, 100)
	if err != nil {
		t.Fatalf("ObservationsNeedingEmbedding: %v", err)
	}
	got := map[int64]bool{}
	for _, r := range results {
		got[r.ID] = true
	}
	if !got[neverEmbedded.ID] {
		t.Errorf("ObservationsNeedingEmbedding missed the never-embedded row (id=%d)", neverEmbedded.ID)
	}
	if !got[wrongDims.ID] {
		t.Errorf("ObservationsNeedingEmbedding missed the wrong-dims row (id=%d)", wrongDims.ID)
	}
	if got[correctlyEmbedded.ID] {
		t.Errorf("ObservationsNeedingEmbedding wrongly included the already-correctly-embedded row (id=%d)", correctlyEmbedded.ID)
	}
	if len(results) != 2 {
		t.Errorf("ObservationsNeedingEmbedding returned %d rows, want exactly 2: %+v", len(results), results)
	}
}

func TestObservationsNeedingEmbeddingScopesToProject(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert("s1", "proj-a", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), memory.Observation{Type: "discovery", Title: "a"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", "proj-b", "Bash", memory.ContentHash("s1", "Bash", "b", "2"), memory.Observation{Type: "discovery", Title: "b"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.ObservationsNeedingEmbedding("proj-a", 768, 0, 100)
	if err != nil {
		t.Fatalf("ObservationsNeedingEmbedding: %v", err)
	}
	if len(results) != 1 || results[0].Project != "proj-a" {
		t.Fatalf("ObservationsNeedingEmbedding(project=proj-a) = %+v, want exactly one row from proj-a", results)
	}
}
