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
	//
	// Scoped to this test's own project: this is a persistent, shared
	// database across every test run ever executed against it, and title
	// "claude-mem installation found" is seeded repeatedly across runs — an
	// unscoped search plus a small LIMIT can genuinely miss this run's own
	// row under accumulated history. Real callers hit the identical
	// scoping requirement for the identical reason (see Search's doc
	// comment), so this isn't a test-only workaround.
	results, err := st.Search(project, "claude-mem", 10)
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

	results, err := st.Search(project, "xyzzy_no_such_term_anywhere", 10)
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
	// Scoped to this test's own project: same accumulated-history reasoning
	// as the hyphenated-query test above — an unscoped comparison set only
	// grows every time this suite runs against the shared instance, and a
	// fixed limit=50 would eventually push one of these two rows out of
	// the returned set on ranking ties alone.
	matches, err := st.SemanticSearch(project, query, 50)
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

func TestPostgresRecentByProjectOrdersNewestFirst(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	older, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "1", project), store.Observation{Type: "discovery", Title: "older"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	newer, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "2", project), store.Observation{Type: "discovery", Title: "newer"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.RecentByProject(project, 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("RecentByProject returned %d results, want 2", len(results))
	}
	if results[0].ID != newer.ID || results[1].ID != older.ID {
		t.Fatalf("RecentByProject order = [%d, %d], want newest first [%d, %d]",
			results[0].ID, results[1].ID, newer.ID, older.ID)
	}
}

// TestPostgresRecentByProjectHandlesNullNarrative is the same regression
// test as the SQLite backend's: title/subtitle/narrative are nullable, and
// the scan code must not assume otherwise.
func TestPostgresRecentByProjectHandlesNullNarrative(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	hash := store.ContentHash("s1", "Bash", "null-case", project)

	if _, err := st.db.Exec(
		`INSERT INTO observations (session_id, project, tool_name, type, title, content_hash, created_at_epoch)
		 VALUES ('s1', $1, 'Bash', 'discovery', 'title only, no subtitle or narrative', $2, 1)`,
		project, hash,
	); err != nil {
		t.Fatalf("insert row with NULL narrative/subtitle: %v", err)
	}

	results, err := st.RecentByProject(project, 10)
	if err != nil {
		t.Fatalf("RecentByProject with a NULL narrative row: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Observation.Narrative != "" {
		t.Fatalf("Narrative = %q, want empty string for a NULL column", results[0].Observation.Narrative)
	}
}

func TestPostgresBySessionIDOrdersOldestFirst(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())

	first, err := st.Insert(sessionID, project, "Bash", store.ContentHash(sessionID, "Bash", "1", project), store.Observation{Type: "discovery", Title: "first"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	second, err := st.Insert(sessionID, project, "Bash", store.ContentHash(sessionID, "Bash", "2", project), store.Observation{Type: "discovery", Title: "second"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.BySessionID(sessionID, 10)
	if err != nil {
		t.Fatalf("BySessionID: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("BySessionID returned %d results, want 2", len(results))
	}
	if results[0].ID != first.ID || results[1].ID != second.ID {
		t.Fatalf("BySessionID order = [%d, %d], want oldest first [%d, %d]",
			results[0].ID, results[1].ID, first.ID, second.ID)
	}
}

func TestPostgresObservationsForFileMatchesReadAndModifiedExactly(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	readMatch, err := st.Insert("s1", project, "Read", store.ContentHash("s1", "Read", "1", project),
		store.Observation{Type: "discovery", Title: "read match", FilesRead: []string{"main.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	modifiedMatch, err := st.Insert("s1", project, "Edit", store.ContentHash("s1", "Edit", "2", project),
		store.Observation{Type: "change", Title: "modified match", FilesModified: []string{"main.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", project, "Read", store.ContentHash("s1", "Read", "3", project),
		store.Observation{Type: "discovery", Title: "substring only", FilesRead: []string{"not-main.go-really"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.ObservationsForFile(project, "main.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ObservationsForFile(\"main.go\") returned %d results, want exactly 2 (read + modified matches, not the substring-only row)", len(results))
	}
	ids := map[int64]bool{results[0].ID: true, results[1].ID: true}
	if !ids[readMatch.ID] || !ids[modifiedMatch.ID] {
		t.Fatalf("results = %v, want both %d and %d", ids, readMatch.ID, modifiedMatch.ID)
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

// TestPostgresOpenRecordsMigrationAndReopenDoesNotReapply is the Postgres
// half of the schema-versioning framework's regression coverage (see
// migrate/migrate_test.go and store/migrations_test.go for the shared
// runner and the SQLite side) — this backend never had ANY migration
// tracking before, just an unconditional idempotent schema block re-run on
// every Open. Confirms schema_migrations actually gets populated against
// a real container, and that reopening doesn't insert a duplicate row.
func TestPostgresOpenRecordsMigrationAndReopenDoesNotReapply(t *testing.T) {
	st := openTestStore(t)

	var count1 int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&count1); err != nil {
		t.Fatalf("count schema_migrations after first Open: %v", err)
	}
	if count1 != 1 {
		t.Fatalf("schema_migrations has %d rows for version 1, want exactly 1", count1)
	}
	st.Close()

	st2, err := Open(context.Background(), testDSN(), DefaultEmbedDims)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer st2.Close()

	var count2 int
	if err := st2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&count2); err != nil {
		t.Fatalf("count schema_migrations after second Open: %v", err)
	}
	if count2 != 1 {
		t.Fatalf("schema_migrations has %d rows for version 1 after reopening, want 1 (no duplicate from re-applying)", count2)
	}
}

// TestPostgresOpenBoundsConnectionPool confirms Open doesn't leave the
// pool at database/sql's default of unlimited — real risk now that every
// long-lived caller (worker.Daemon, mcpserver.Server) opens one Store for
// its whole process lifetime instead of one per event: an unbounded pool
// is the only thing standing between a burst of concurrent calls and
// Postgres's own max_connections, shared with every other client on the
// same server.
func TestPostgresOpenBoundsConnectionPool(t *testing.T) {
	st := openTestStore(t)
	stats := st.db.Stats()
	if stats.MaxOpenConnections <= 0 {
		t.Fatalf("MaxOpenConnections = %d, want a positive bound, not database/sql's default of unlimited (0)", stats.MaxOpenConnections)
	}
}
