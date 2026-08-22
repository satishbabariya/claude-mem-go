package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestStatsDescribesWhatTheStoreContains covers the observability gap
// this exists for: every check `doctor` ran was a reachability check, so
// it could report "All critical checks passed" while capture had been
// dead for weeks. Nothing measured whether anything was being remembered.
func TestStatsDescribesWhatTheStoreContains(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	types := []string{"discovery", "change", "decision", "summary"}
	for i := 0; i < 8; i++ {
		title := fmt.Sprintf("row %d", i)
		if _, err := st.Insert(context.Background(), fmt.Sprintf("sess-%d", i%3), fmt.Sprintf("proj-%d", i%2), "Bash",
			memory.ContentHash("s", "Bash", title, fmt.Sprint(i)),
			memory.Observation{Type: types[i%4], Title: title}, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	// Embed only some of them: the gap between Observations and Embedded
	// is exactly what semantic search cannot see, and is reported for
	// that reason.
	// A real project name, not "": unlike Search, RecentByProject uses a
	// plain `WHERE project = ?`, so an empty string matches nothing —
	// a documented asymmetry between the two, and one this test tripped
	// over on the first run.
	rows, err := st.RecentByProject(context.Background(), "proj-0", 3)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("seeding embeddings: got %d rows, want 3", len(rows))
	}
	for _, r := range rows {
		if err := st.SaveEmbedding(context.Background(), r.ID, make([]float32, 768)); err != nil {
			t.Fatalf("SaveEmbedding: %v", err)
		}
	}

	s, err := st.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Observations != 8 {
		t.Fatalf("Observations = %d, want 8", s.Observations)
	}
	if s.Projects != 2 {
		t.Fatalf("Projects = %d, want 2", s.Projects)
	}
	if s.Sessions != 3 {
		t.Fatalf("Sessions = %d, want 3", s.Sessions)
	}
	if s.Embedded != 3 {
		t.Fatalf("Embedded = %d, want 3 — the un-embedded remainder is what semantic search misses", s.Embedded)
	}
	if s.ByType["discovery"] != 2 || s.ByType["summary"] != 2 {
		t.Fatalf("ByType = %v, want 2 of each of 4 types", s.ByType)
	}
	if s.OldestEpochMs == 0 || s.NewestEpochMs == 0 {
		t.Fatalf("Oldest/Newest = %d/%d, want real timestamps", s.OldestEpochMs, s.NewestEpochMs)
	}
	if s.NewestEpochMs < s.OldestEpochMs {
		t.Fatalf("Newest (%d) is older than Oldest (%d)", s.NewestEpochMs, s.OldestEpochMs)
	}
	if age := time.Since(time.UnixMilli(s.NewestEpochMs)); age > time.Hour {
		t.Fatalf("newest observation reads as %v old — the recency signal is the whole point", age)
	}
}

// TestStatsOnAnEmptyStoreReportsZeroNotEpochZero guards the distinction
// between "nothing recorded yet" and "recorded in 1970". min()/max() over
// no rows is SQL NULL, and scanning that into an int64 without care
// yields 0, which formats as 1970-01-01 and reads as a corrupt store.
func TestStatsOnAnEmptyStoreReportsZeroNotEpochZero(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	s, err := st.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats on an empty store: %v", err)
	}
	if s.Observations != 0 || s.Projects != 0 || s.Sessions != 0 || s.Embedded != 0 {
		t.Fatalf("empty store reported %+v, want all zeros", s)
	}
	if s.OldestEpochMs != 0 || s.NewestEpochMs != 0 {
		t.Fatalf("empty store reported timestamps %d/%d, want 0 for both", s.OldestEpochMs, s.NewestEpochMs)
	}
	if s.ByType == nil {
		t.Fatal("ByType is nil on an empty store — callers must be able to index it without a nil check")
	}
}
