package postgres

import (
	"fmt"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/store"
)

// TestPostgresStatsMatchesTheSQLiteShape is a cross-backend agreement
// test. Stats feeds `doctor` and `stats`, so the two backends disagreeing
// would mean the same store reported differently depending on which one
// an operator happened to be running — and cross-backend divergence has
// been a repeated source of real bugs in this project.
func TestPostgresStatsMatchesTheSQLiteShape(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	types := []string{"discovery", "change", "decision", "summary"}
	for i := 0; i < 8; i++ {
		title := fmt.Sprintf("stats row %d", i)
		if _, err := st.Insert(fmt.Sprintf("%s-sess-%d", project, i%3), project, "Bash",
			store.ContentHash("s", "Bash", title, project),
			store.Observation{Type: types[i%4], Title: title}, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	s, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// This is a shared, persistent database, so absolute totals belong to
	// every run that came before. Assert the shape and the invariants
	// instead, which is what actually has to hold.
	if s.Observations < 8 {
		t.Fatalf("Observations = %d, want at least the 8 just inserted", s.Observations)
	}
	if s.Projects < 1 || s.Sessions < 3 {
		t.Fatalf("Projects=%d Sessions=%d, want at least 1 and 3", s.Projects, s.Sessions)
	}
	if s.ByType == nil {
		t.Fatal("ByType is nil — callers must be able to index it without a nil check")
	}
	for _, ty := range types {
		if s.ByType[ty] < 2 {
			t.Fatalf("ByType[%s] = %d, want at least the 2 just inserted", ty, s.ByType[ty])
		}
	}
	if s.Embedded > s.Observations {
		t.Fatalf("Embedded (%d) exceeds Observations (%d)", s.Embedded, s.Observations)
	}
	if s.NewestEpochMs < s.OldestEpochMs {
		t.Fatalf("Newest (%d) older than Oldest (%d)", s.NewestEpochMs, s.OldestEpochMs)
	}
	if s.NewestEpochMs == 0 {
		t.Fatal("NewestEpochMs is 0 on a non-empty store — that formats as 1970 and reads as corruption")
	}
}
