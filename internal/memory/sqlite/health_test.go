package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestHealthDetailsReflectsRealPragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["journal_mode"] != "wal" {
		t.Errorf("journal_mode = %q, want wal", details["journal_mode"])
	}
	if details["foreign_keys"] != "1" {
		t.Errorf("foreign_keys = %q, want 1", details["foreign_keys"])
	}
	if details["busy_timeout_ms"] != "5000" {
		t.Errorf("busy_timeout_ms = %q, want 5000", details["busy_timeout_ms"])
	}
	if _, ok := details["embedding_dims"]; ok {
		t.Errorf("embedding_dims present with no embedded observations at all, want it omitted: %v", details)
	}
}

// TestHealthDetailsFlagsInconsistentEmbeddingDimensions is the regression
// test for the real silent-failure mode this key exists to surface: if
// the configured Ollama embedding model ever changes (768 dims to some
// other model's dims, say), SemanticSearch's cosineSimilarity returns -1
// on any length mismatch rather than erroring — the old embeddings just
// quietly stop ever matching a new-model query, forever, with nothing
// anywhere saying so. Reproduced directly here: two real observations,
// saved with genuinely different embedding lengths via the real
// SaveEmbedding path, not a hand-crafted row.
func TestHealthDetailsFlagsInconsistentEmbeddingDimensions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	r1, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), memory.Observation{Type: "discovery", Title: "old model"}, 0)
	if err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	if err := st.SaveEmbedding(r1.ID, make([]float32, 768)); err != nil {
		t.Fatalf("SaveEmbedding 1: %v", err)
	}
	r2, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "b", "2"), memory.Observation{Type: "discovery", Title: "new model"}, 0)
	if err != nil {
		t.Fatalf("Insert 2: %v", err)
	}
	if err := st.SaveEmbedding(r2.ID, make([]float32, 384)); err != nil {
		t.Fatalf("SaveEmbedding 2: %v", err)
	}

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["embedding_dims_consistent"] != "false" {
		t.Errorf("embedding_dims_consistent = %q, want false (two different dimensions are actually present)", details["embedding_dims_consistent"])
	}
	if details["embedding_dims"] != "384:1,768:1" {
		t.Errorf("embedding_dims = %q, want %q", details["embedding_dims"], "384:1,768:1")
	}
}

func TestHealthDetailsReportsConsistentEmbeddingDimensions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	r1, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), memory.Observation{Type: "discovery", Title: "one"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(r1.ID, make([]float32, 768)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["embedding_dims_consistent"] != "true" {
		t.Errorf("embedding_dims_consistent = %q, want true", details["embedding_dims_consistent"])
	}
	if details["embedding_dims"] != "768:1" {
		t.Errorf("embedding_dims = %q, want %q", details["embedding_dims"], "768:1")
	}
}
