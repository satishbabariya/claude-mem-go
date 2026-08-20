package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"claude-mem-go/store"
)

// testDSN points at docker-compose.yml's postgres service by default —
// override with CLAUDE_MEM_GO_TEST_POSTGRES_DSN for a different instance.
func testDSN() string {
	if dsn := os.Getenv("CLAUDE_MEM_GO_TEST_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://claudemem:claudemem@localhost:55432/claudemem?sslmode=disable"
}

// openTestStore skips (not fails) the test when Postgres isn't reachable —
// this backend needs Docker running, unlike every other package in this
// project, and a missing dependency should skip cleanly rather than break
// `go test ./...` for someone who hasn't started it.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := Open(ctx, testDSN(), DefaultEmbedDims)
	if err != nil {
		t.Skipf("postgres not reachable at %s (start it with `docker compose up -d`): %v", testDSN(), err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// uniqueProject avoids collisions between test runs against the same
// long-lived database — this isn't a fresh temp file like the SQLite tests
// get, it's a shared Postgres instance.
func uniqueProject(t *testing.T) string {
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

func TestPostgresInsertIsIdempotentOnContentHash(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	// Postgres here is a persistent, shared instance (unlike the SQLite
	// tests' fresh temp file per run) — the hash must be unique to THIS run,
	// or re-running the suite collides with a previous run's row and this
	// test would see a false "duplicate" on its very first Insert.
	hash := store.ContentHash("s1", "Bash", "npm test", project)
	o := store.Observation{Type: "discovery", Title: "Tests passed"}

	first, err := st.Insert("s1", project, "Bash", hash, o, 0.01)
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first Insert of a fresh content_hash: want Inserted=true")
	}

	second, err := st.Insert("s1", project, "Bash", hash, o, 0.01)
	if err != nil {
		t.Fatalf("second Insert (duplicate): %v", err)
	}
	if second.Inserted {
		t.Fatal("second Insert with the same content_hash: want Inserted=false")
	}
	if second.ID != first.ID {
		t.Fatalf("second Insert returned ID %d, want %d", second.ID, first.ID)
	}

	count, err := st.CountByProject(project)
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject = %d, want 1", count)
	}
}

func TestPostgresSearchHandlesHyphenatedQueries(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	o := store.Observation{Type: "discovery", Title: "claude-mem installation found"}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The exact real bug found in the SQLite/FTS5 backend: "claude-mem"
	// broke FTS5's grammar outright. Postgres's plainto_tsquery must not
	// have the same problem — that's the whole reason this backend exists.
	results, err := st.Search("claude-mem", 10)
	if err != nil {
		t.Fatalf("Search(\"claude-mem\") returned an error: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Project == project {
			found = true
		}
	}
	if !found {
		t.Fatalf("Search(\"claude-mem\") did not find the seeded row in project %s among %d results", project, len(results))
	}
}

func TestPostgresSearchRankingAndNoMatch(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	o := store.Observation{Type: "discovery", Title: "A totally unrelated observation about kites"}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.Search("xyzzy_no_such_term_anywhere", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if r.Project == project {
			t.Fatalf("Search for an unrelated term matched project %s — false positive", project)
		}
	}
}

func TestPostgresSemanticSearchOrdersByCosineSimilarity(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	oCat, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "cat", project), store.Observation{Type: "discovery", Title: "about cats"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	oDog, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "dog", project), store.Observation{Type: "discovery", Title: "about dogs"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	dims := make([]float32, DefaultEmbedDims)
	dims[0] = 1
	if err := st.SaveEmbedding(oCat.ID, dims); err != nil {
		t.Fatalf("SaveEmbedding cat: %v", err)
	}
	orth := make([]float32, DefaultEmbedDims)
	orth[1] = 1
	if err := st.SaveEmbedding(oDog.ID, orth); err != nil {
		t.Fatalf("SaveEmbedding dog: %v", err)
	}

	query := make([]float32, DefaultEmbedDims)
	query[0] = 1
	matches, err := st.SemanticSearch(query, 50)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}

	var catRank, dogRank = -1, -1
	for i, m := range matches {
		if m.ID == oCat.ID {
			catRank = i
		}
		if m.ID == oDog.ID {
			dogRank = i
		}
	}
	if catRank == -1 || dogRank == -1 {
		t.Fatalf("both seeded observations should appear in results; catRank=%d dogRank=%d", catRank, dogRank)
	}
	if catRank >= dogRank {
		t.Fatalf("the aligned vector (cat, rank %d) should rank ahead of the orthogonal one (dog, rank %d)", catRank, dogRank)
	}
}

func TestPostgresSemanticSearchDimensionMismatchErrors(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	o, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "c", project), store.Observation{Type: "discovery", Title: "x"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Wrong dimensionality must fail loudly, not silently truncate/pad —
	// this is the schema's fixed-dims guarantee actually being enforced.
	if err := st.SaveEmbedding(o.ID, []float32{1, 2, 3}); err == nil {
		t.Fatal("SaveEmbedding with the wrong vector dimensionality: want an error, got nil")
	}
}
