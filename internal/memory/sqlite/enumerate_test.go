package sqlite

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func seedForEnumeration(t *testing.T, n int) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "enum.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("step %d", i)
		if _, err := st.Insert("s1", "enum-proj", "Bash",
			memory.ContentHash("s1", "Bash", title, fmt.Sprint(i)),
			memory.Observation{Type: "change", Title: title}, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	return st
}

// TestSearchWithNoQueryEnumerates is the regression test for a real hole
// in the read surface: there was NO way to walk a project's history.
//
// search_observations required a query — an empty one, and "*", both
// returned nothing — and recent_observations has no offset and stops at
// 100. Measured against a 150-observation project: 50 observations were
// simply unreachable through the MCP surface, and a date-bounded
// enumeration (the thing a timeline or weekly-digest report is built
// from, both of which real claude-mem ships as first-class skills) could
// not be expressed at all.
func TestSearchWithNoQueryEnumerates(t *testing.T) {
	st := seedForEnumeration(t, 150)

	page1, err := st.Search("enum-proj", "", "", 100, 0, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("enumerate page 1: %v", err)
	}
	page2, err := st.Search("enum-proj", "", "", 100, 100, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("enumerate page 2: %v", err)
	}
	if len(page1) != 100 || len(page2) != 50 {
		t.Fatalf("paged enumeration returned %d + %d, want 100 + 50 — the whole point is reaching all 150",
			len(page1), len(page2))
	}
	if page1[0].Observation.Title != "step 0" {
		t.Fatalf("first enumerated row = %q, want %q under date_asc", page1[0].Observation.Title, "step 0")
	}
	if last := page2[len(page2)-1]; last.Observation.Title != "step 149" {
		t.Fatalf("last enumerated row = %q, want %q — the tail is exactly what was unreachable before",
			last.Observation.Title, "step 149")
	}
}

// TestSearchWithNoQueryDefaultsToNewestFirst pins the ordering fallback.
// "Relevance" is not merely a poor default without a query — the SQLite
// order clause references `rank`, which does not exist outside an FTS
// MATCH, so asking for it on this path is a SQL error.
func TestSearchWithNoQueryDefaultsToNewestFirst(t *testing.T) {
	st := seedForEnumeration(t, 5)

	got, err := st.Search("enum-proj", "", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("enumerate with the default order: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows, want 5", len(got))
	}
	if got[0].Observation.Title != "step 4" {
		t.Fatalf("first row = %q, want %q — an empty orderBy must fall back to newest-first, "+
			"matching RecentByProject, not to a rank expression that cannot be evaluated here",
			got[0].Observation.Title, "step 4")
	}
}

// TestSearchWithNoQueryStillHonorsFilters guards the obvious way to get
// enumeration wrong: dropping the FTS predicate but also dropping the
// scope clauses with it, turning every enumeration into "the whole store".
func TestSearchWithNoQueryStillHonorsFilters(t *testing.T) {
	st := seedForEnumeration(t, 10)
	if _, err := st.Insert("s2", "other-proj", "Bash",
		memory.ContentHash("s2", "Bash", "elsewhere", "x"),
		memory.Observation{Type: "discovery", Title: "elsewhere"}, 0); err != nil {
		t.Fatalf("Insert other project: %v", err)
	}

	scoped, err := st.Search("enum-proj", "", "", 100, 0, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("scoped enumerate: %v", err)
	}
	if len(scoped) != 10 {
		t.Fatalf("project-scoped enumeration returned %d rows, want 10 — it leaked another project", len(scoped))
	}

	typed, err := st.Search("", "", "discovery", 100, 0, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("typed enumerate: %v", err)
	}
	if len(typed) != 1 || typed[0].Observation.Title != "elsewhere" {
		t.Fatalf("type-filtered enumeration returned %d rows (%v), want exactly the 1 discovery", len(typed), typed)
	}
}

// TestSearchWithAQueryStillSearches is the counterweight: an enumeration
// path that accidentally swallowed the query would make every search
// return the entire store, which would look like "more results" rather
// than like a bug.
func TestSearchWithAQueryStillSearches(t *testing.T) {
	st := seedForEnumeration(t, 10)

	got, err := st.Search("enum-proj", "step 3", "", 100, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("keyword search returned nothing — enumeration broke real searching")
	}
	if len(got) == 10 {
		t.Fatal("keyword search returned every row — the query is being ignored and every search is now an enumeration")
	}
}
