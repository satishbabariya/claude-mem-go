package postgres

import (
	"context"
	"testing"
	"time"

	"claude-mem-go/store"
)

// TestHealthDetailsReflectsRealPoolAndSchemaState is verified against the
// live Docker container, not fabricated values — pool stats come from
// database/sql's own accounting, and hnsw_index_exists/vector_extension
// are real queries against the schema Open actually created.
func TestHealthDetailsReflectsRealPoolAndSchemaState(t *testing.T) {
	st := openTestStore(t)

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["hnsw_index_exists"] != "true" {
		t.Errorf("hnsw_index_exists = %q, want true — Open's schema always creates it", details["hnsw_index_exists"])
	}
	if details["vector_extension"] == "" || details["vector_extension"] == "not installed" {
		t.Errorf("vector_extension = %q, want a real version string (pgvector/pgvector:pg16 has it installed)", details["vector_extension"])
	}
	if details["pool_max_open_connections"] != "10" {
		t.Errorf("pool_max_open_connections = %q, want 10 (Open's own SetMaxOpenConns bound)", details["pool_max_open_connections"])
	}
	// pool_open_connections must be a real, non-negative count reflecting
	// this very call having just used a connection — not asserting an
	// exact number since pool behavior is inherently a little timing-
	// dependent, just that it's present and sane.
	if details["pool_open_connections"] == "" {
		t.Error("pool_open_connections is empty, want a real count")
	}
	if details["hnsw_ef_search"] != "default (40)" {
		t.Errorf(`hnsw_ef_search = %q, want "default (40)" — openTestStore configures no override`, details["hnsw_ef_search"])
	}
}

// TestHealthDetailsReflectsConfiguredHNSWEfSearch is the regression test
// for a real observability gap: doctor had no visibility into whether an
// operator's -hnsw-ef-search override was actually configured at all,
// the exact tuning knob a previous pass added and found Postgres doesn't
// reliably self-report. hnsw_ef_search reports THIS Store's configured
// value, not a live Postgres session setting — there isn't one to read,
// since SemanticSearch applies it per-call via a transaction-scoped SET
// LOCAL rather than a persistent session GUC (see SemanticSearch's own
// doc comment).
func TestHealthDetailsReflectsConfiguredHNSWEfSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := Open(ctx, testDSN(), DefaultEmbedDims, 250)
	if err != nil {
		t.Skipf("postgres not reachable at %s: %v", testDSN(), err)
	}
	defer st.Close()

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["hnsw_ef_search"] != "250" {
		t.Errorf(`hnsw_ef_search = %q, want "250" (the value this Store was opened with)`, details["hnsw_ef_search"])
	}
}

// TestSaveEmbeddingRejectsWrongDimensionVector confirms this backend's
// structural advantage over SQLite's: the embedding column is a fixed
// vector(N) type set once at schema creation, so a dimension mismatch
// (e.g. from switching Ollama embedding models) fails loudly right here
// instead of silently degrading every future SemanticSearch call the way
// it can on SQLite (see store.TestHealthDetailsFlagsInconsistentEmbeddingDimensions
// for that side of the same real risk).
func TestSaveEmbeddingRejectsWrongDimensionVector(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	res, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project),
		store.Observation{Type: "discovery", Title: "dim mismatch test"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	wrongDims := make([]float32, DefaultEmbedDims/2)
	if err := st.SaveEmbedding(res.ID, wrongDims); err == nil {
		t.Fatalf("SaveEmbedding with %d dims (schema expects %d): want an error, got nil — a silent dimension mismatch here would be worse than SQLite's, since it wouldn't even show up in HealthDetails afterward", len(wrongDims), DefaultEmbedDims)
	}
}

// TestHealthDetailsReportsEmbeddingDimsForPostgres confirms the same
// embedding_dims key the SQLite backend reports also appears here, for
// parity — always consistent by construction, since the column type
// itself won't allow otherwise.
func TestHealthDetailsReportsEmbeddingDimsForPostgres(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	res, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project),
		store.Observation{Type: "discovery", Title: "embedded row"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.SaveEmbedding(res.ID, make([]float32, DefaultEmbedDims)); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["embedding_dims_consistent"] != "true" {
		t.Errorf("embedding_dims_consistent = %q, want true (this backend cannot have mixed dimensions)", details["embedding_dims_consistent"])
	}
	if details["embedding_dims"] == "" {
		t.Error("embedding_dims is empty, want a real histogram entry now that at least one row is embedded")
	}
}
