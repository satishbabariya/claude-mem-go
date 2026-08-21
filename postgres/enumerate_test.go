package postgres

import (
	"fmt"
	"testing"

	"claude-mem-go/store"
)

// TestPostgresSearchWithNoQueryEnumerates is the Postgres half of the
// same fix, run against the real container. The two backends must agree:
// an empty query enumerates rather than matching nothing, and the other
// filters still apply.
//
// The Postgres path has its own trap, which is why this is not merely a
// copy of the SQLite test: `search_vector` is NULL for a row whose
// indexed text is empty, so passing a "match everything" tsquery would
// silently drop exactly the rows enumeration exists to include. The
// predicate is dropped entirely instead.
func TestPostgresSearchWithNoQueryEnumerates(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	const n = 150
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("step %d", i)
		// The project must be part of the hash. Postgres here is a
		// persistent, shared instance, so a hash built only from the
		// title collides with the PREVIOUS run's row — Insert dedups,
		// the rows stay under the old run's project name, and this test
		// then enumerates its own (empty) project. Found exactly that
		// way: it passed on a fresh database and failed on every re-run.
		if _, err := st.Insert("s-enum", project, "Bash",
			store.ContentHash("s-enum", "Bash", title, project+fmt.Sprint(i)),
			store.Observation{Type: "change", Title: title}, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	page1, err := st.Search(project, "", "", 100, 0, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("enumerate page 1: %v", err)
	}
	page2, err := st.Search(project, "", "", 100, 100, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("enumerate page 2: %v", err)
	}
	if len(page1) != 100 || len(page2) != 50 {
		t.Fatalf("paged enumeration returned %d + %d, want 100 + 50", len(page1), len(page2))
	}
	if page1[0].Observation.Title != "step 0" {
		t.Fatalf("first row = %q, want %q", page1[0].Observation.Title, "step 0")
	}
	if last := page2[len(page2)-1]; last.Observation.Title != "step 149" {
		t.Fatalf("last row = %q, want %q", last.Observation.Title, "step 149")
	}

	t.Run("default order is newest-first", func(t *testing.T) {
		// The relevance clause here is ts_rank_cd(..., $1); on the
		// enumeration path $1 does not exist, so falling through to it
		// would be a SQL error rather than a bad sort.
		got, err := st.Search(project, "", "", 5, 0, 0, 0, "")
		if err != nil {
			t.Fatalf("enumerate with the default order: %v", err)
		}
		if len(got) == 0 || got[0].Observation.Title != "step 149" {
			t.Fatalf("first row = %v, want %q", got, "step 149")
		}
	})

	t.Run("project scope still applies", func(t *testing.T) {
		other := uniqueProject(t)
		if _, err := st.Insert("s-other", other, "Bash",
			store.ContentHash("s-other", "Bash", "elsewhere", other),
			store.Observation{Type: "discovery", Title: "elsewhere"}, 0); err != nil {
			t.Fatalf("Insert other: %v", err)
		}
		got, err := st.Search(project, "", "", 500, 0, 0, 0, "date_asc")
		if err != nil {
			t.Fatalf("scoped enumerate: %v", err)
		}
		if len(got) != n {
			t.Fatalf("scoped enumeration returned %d rows, want %d — it leaked another project", len(got), n)
		}
	})

	t.Run("a real query still searches", func(t *testing.T) {
		got, err := st.Search(project, "step 3", "", 500, 0, 0, 0, "")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("keyword search returned nothing — enumeration broke searching")
		}
		if len(got) == n {
			t.Fatal("keyword search returned every row — the query is being ignored")
		}
	})
}
