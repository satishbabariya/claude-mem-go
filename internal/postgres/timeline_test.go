package postgres

import (
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/store"
)

func seedSequence(t *testing.T, st *Store, project string, n int) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		title := string(rune('A' + i))
		res, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", project, title+t.Name()), store.Observation{Type: "discovery", Title: title}, 0)
		if err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
		ids[i] = res.ID
	}
	return ids
}

// TestPostgresTimelineMatchesSQLiteBehavior confirms this backend's
// Timeline gives the identical chronological before/anchor/after shape
// against the real container — BIGSERIAL's monotonic-with-insertion-order
// property (the same assumption the SQLite backend's AUTOINCREMENT rowid
// relies on) verified for real here, not assumed to just work because
// SQLite's did.
func TestPostgresTimelineMatchesSQLiteBehavior(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	ids := seedSequence(t, st, project, 7) // A..G
	anchor := ids[3]                       // D

	results, err := st.Timeline(project, anchor, 2, 2)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	var titles []string
	for _, r := range results {
		titles = append(titles, r.Observation.Title)
	}
	want := []string{"B", "C", "D", "E", "F"}
	if len(titles) != len(want) {
		t.Fatalf("Timeline titles = %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("Timeline titles = %v, want %v", titles, want)
		}
	}
}

func TestPostgresTimelineRejectsAnchorFromADifferentProject(t *testing.T) {
	st := openTestStore(t)
	projectA := uniqueProject(t)
	projectB := uniqueProject(t)

	idsA := seedSequence(t, st, projectA, 1)
	seedSequence(t, st, projectB, 1)

	if _, err := st.Timeline(projectB, idsA[0], 1, 1); err == nil {
		t.Fatal("Timeline with an anchor from a different project: want an error, got nil — a real cross-project leak otherwise")
	}
}

// TestPostgresTimelineNegativeDepthDoesNotError confirms the clamp here
// too: unlike SQLite (where a negative LIMIT silently means "unlimited"),
// Postgres's own LIMIT rejects a negative value outright with "LIMIT must
// not be negative" (confirmed against the live container) — a naive
// passthrough would surface that raw driver error to whatever called
// Timeline instead of just returning the anchor alone.
func TestPostgresTimelineNegativeDepthDoesNotError(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	ids := seedSequence(t, st, project, 5)
	anchor := ids[2]

	results, err := st.Timeline(project, anchor, -1, -1)
	if err != nil {
		t.Fatalf("Timeline with negative depths: want a clean clamp, got an error: %v", err)
	}
	if len(results) != 1 || results[0].ID != anchor {
		t.Fatalf("Timeline(depthBefore=-1, depthAfter=-1) = %+v, want exactly the anchor alone", results)
	}
}
