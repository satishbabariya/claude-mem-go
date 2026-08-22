package postgres

import (
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestPostgresObservationsNeedingEmbeddingFindsUnembeddedRows(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	neverEmbedded, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "a", project), memory.Observation{Type: "discovery", Title: "never embedded"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	correctlyEmbedded, err := st.Insert("s1", project, "Bash", memory.ContentHash("s1", "Bash", "b", project), memory.Observation{Type: "discovery", Title: "already fine"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(correctlyEmbedded.ID, make([]float32, DefaultEmbedDims)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	results, err := st.ObservationsNeedingEmbedding(project, DefaultEmbedDims, 0, 100)
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
	if got[correctlyEmbedded.ID] {
		t.Errorf("ObservationsNeedingEmbedding wrongly included the already-correctly-embedded row (id=%d)", correctlyEmbedded.ID)
	}
}
