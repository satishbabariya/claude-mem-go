package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestRecentByProjectOrdersNewestFirstAndScopesToProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	older, err := st.Insert(context.Background(), "s1", "proj-a", "Bash", memory.ContentHash("s1", "Bash", "1", "1"), memory.Observation{Type: "discovery", Title: "older"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	newer, err := st.Insert(context.Background(), "s1", "proj-a", "Bash", memory.ContentHash("s1", "Bash", "2", "2"), memory.Observation{Type: "discovery", Title: "newer"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// A different project must never leak into proj-a's recent list.
	if _, err := st.Insert(context.Background(), "s1", "proj-b", "Bash", memory.ContentHash("s1", "Bash", "3", "3"), memory.Observation{Type: "discovery", Title: "other project"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.RecentByProject(context.Background(), "proj-a", 10)
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

func TestRecentByProjectRespectsLimit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	for i := 0; i < 5; i++ {
		if _, err := st.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", string(rune('a'+i)), "x"), memory.Observation{Type: "discovery", Title: "x"}, 0); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	results, err := st.RecentByProject(context.Background(), "proj", 3)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("RecentByProject with limit=3 returned %d results", len(results))
	}
}

// TestRecentByProjectHandlesNullNarrative is the regression test for a real
// bug: title/subtitle/narrative are nullable TEXT columns, but the scan
// code originally assumed non-NULL and errored ("converting NULL to string
// is unsupported") the first time a row without one showed up — a
// hand-inserted test row for the SessionStart context-injection feature,
// not something the normal Insert() path produces (it always supplies at
// least an empty string), but the schema permits it regardless.
func TestRecentByProjectHandlesNullNarrative(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.db.Exec(
		`INSERT INTO observations (session_id, project, tool_name, type, title, content_hash, created_at, created_at_epoch)
		 VALUES ('s1', 'proj', 'Bash', 'discovery', 'title only, no subtitle or narrative', 'null-narrative-hash', '2020-01-01T00:00:00Z', 1)`,
	); err != nil {
		t.Fatalf("insert row with NULL narrative/subtitle: %v", err)
	}

	results, err := st.RecentByProject(context.Background(), "proj", 10)
	if err != nil {
		t.Fatalf("RecentByProject with a NULL narrative row: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Observation.Narrative != "" {
		t.Fatalf("Narrative = %q, want empty string for a NULL column", results[0].Observation.Narrative)
	}
	if results[0].Observation.Subtitle != "" {
		t.Fatalf("Subtitle = %q, want empty string for a NULL column", results[0].Observation.Subtitle)
	}
}

func TestBySessionIDOrdersOldestFirstAndScopesToSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	first, err := st.Insert(context.Background(), "session-a", "proj", "Bash", memory.ContentHash("session-a", "Bash", "1", "1"), memory.Observation{Type: "discovery", Title: "first thing"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	second, err := st.Insert(context.Background(), "session-a", "proj", "Bash", memory.ContentHash("session-a", "Bash", "2", "2"), memory.Observation{Type: "discovery", Title: "second thing"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// A different session must never leak into session-a's history.
	if _, err := st.Insert(context.Background(), "session-b", "proj", "Bash", memory.ContentHash("session-b", "Bash", "3", "3"), memory.Observation{Type: "discovery", Title: "other session"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.BySessionID(context.Background(), "", "session-a", 10)
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

// TestBySessionIDScopesToProject: a project scope excludes the session's
// rows recorded under another project; an empty project includes them.
func TestBySessionIDScopesToProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	for i, project := range []string{"proj-a", "proj-b"} {
		hash := memory.ContentHash("session-x", "Bash", fmt.Sprint(i), project)
		if _, err := st.Insert(context.Background(), "session-x", project, "Bash", hash, memory.Observation{Type: "discovery", Title: project}, 0); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	scoped, err := st.BySessionID(context.Background(), "proj-a", "session-x", 10)
	if err != nil {
		t.Fatalf("BySessionID(proj-a): %v", err)
	}
	if len(scoped) != 1 || scoped[0].Project != "proj-a" {
		t.Fatalf("BySessionID(proj-a) = %+v, want exactly the one proj-a row", scoped)
	}
	all, err := st.BySessionID(context.Background(), "", "session-x", 10)
	if err != nil {
		t.Fatalf("BySessionID(\"\"): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("BySessionID(\"\") returned %d rows, want 2 (every project)", len(all))
	}
}

func TestRecentByProjectEmptyForUnknownProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	results, err := st.RecentByProject(context.Background(), "no-such-project", 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("RecentByProject for an unknown project returned %d results, want 0", len(results))
	}
}
