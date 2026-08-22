package sqlite

import (
	"context"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// seedSequence inserts n observations for project, in order, and returns
// their IDs in insertion order — Timeline's ordering guarantees depend on
// id increasing monotonically with insertion, so tests build a known
// sequence rather than asserting against arbitrary pre-existing rows.
func seedSequence(t *testing.T, st *Store, project string, n int) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		title := string(rune('A' + i))
		res, err := st.Insert(context.Background(), "s1", project, "Bash", memory.ContentHash("s1", "Bash", project, title), memory.Observation{Type: "discovery", Title: title}, 0)
		if err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
		ids[i] = res.ID
	}
	return ids
}

func TestTimelineReturnsBeforeAnchorAfterInChronologicalOrder(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ids := seedSequence(t, st, "proj", 7) // titles A..G, ids[3] = "D"
	anchor := ids[3]

	results, err := st.Timeline(context.Background(), "proj", anchor, 2, 2)
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
			t.Errorf("Timeline titles = %v, want %v", titles, want)
			break
		}
	}
}

func TestTimelineClampsAtTheStartAndEndOfHistory(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ids := seedSequence(t, st, "proj", 3) // A, B, C

	// Anchor on the very first row, asking for more "before" than exists.
	results, err := st.Timeline(context.Background(), "proj", ids[0], 5, 5)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	var titles []string
	for _, r := range results {
		titles = append(titles, r.Observation.Title)
	}
	want := []string{"A", "B", "C"}
	if len(titles) != len(want) || titles[0] != "A" || titles[1] != "B" || titles[2] != "C" {
		t.Fatalf("Timeline titles = %v, want %v (no crash/negative-limit behavior asking for more history than exists)", titles, want)
	}
}

func TestTimelineRejectsAnchorFromADifferentProject(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	idsA := seedSequence(t, st, "proj-a", 2)
	seedSequence(t, st, "proj-b", 2)

	if _, err := st.Timeline(context.Background(), "proj-b", idsA[0], 1, 1); err == nil {
		t.Fatal("Timeline with an anchor from proj-a but project=proj-b: want an error, got nil — a real cross-project leak otherwise")
	}
}

func TestTimelineUnknownAnchorIsAnError(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Timeline(context.Background(), "proj", 999999, 1, 1); err == nil {
		t.Fatal("Timeline with an unknown anchor id: want an error, got nil")
	}
}

func TestTimelineNeverCrossesIntoAnotherProjectsRows(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// proj-a: one row. proj-b inserted immediately after (so its ids are
	// numerically adjacent to proj-a's) then another proj-a row. A naive
	// id-only WHERE clause with no project filter would pull proj-b's row
	// into proj-a's timeline; the project filter in the before/after
	// queries must prevent that.
	idsA1 := seedSequence(t, st, "proj-a", 1)
	seedSequence(t, st, "proj-b", 1)
	idsA2 := seedSequence(t, st, "proj-a", 1)
	_ = idsA1

	results, err := st.Timeline(context.Background(), "proj-a", idsA2[0], 5, 5)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	for _, r := range results {
		if r.Project != "proj-a" {
			t.Fatalf("Timeline for proj-a returned a row from project %q — cross-project leak", r.Project)
		}
	}
}

// TestTimelineNegativeDepthDoesNotReturnUnlimitedRows is the regression
// test for a real bug found against this project's own SQLite driver:
// SQLite's LIMIT treats a negative value as "unlimited," so a naive
// `LIMIT ?` with depthBefore=-1 returned EVERY row before the anchor
// instead of zero. mcpserver.go's own caller already defaults <=0 to 3,
// but Timeline itself must not depend on that — any other caller (a
// future CLI command, a test, a bug) could pass a negative value
// directly.
func TestTimelineNegativeDepthDoesNotReturnUnlimitedRows(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ids := seedSequence(t, st, "proj", 10)
	anchor := ids[5]

	results, err := st.Timeline(context.Background(), "proj", anchor, -1, -1)
	if err != nil {
		t.Fatalf("Timeline with negative depths: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Timeline(depthBefore=-1, depthAfter=-1) returned %d rows, want exactly 1 (just the anchor) — negative depths must clamp to 0, not be treated as unlimited", len(results))
	}
	if results[0].ID != anchor {
		t.Fatalf("Timeline with negative depths returned id=%d, want just the anchor id=%d", results[0].ID, anchor)
	}
}
