package store

import "testing"

func TestSanitizeFTSQuery(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"single word", "hello", `"hello"`},
		{"hyphenated word", "claude-mem", `"claude-mem"`},
		{"boolean OR preserved", "monetization OR sqlite", `"monetization" OR "sqlite"`},
		{"boolean AND preserved", "config AND parser", `"config" AND "parser"`},
		{"boolean NOT preserved", "search NOT deprecated", `"search" NOT "deprecated"`},
		{"embedded quote escaped", `say "hi"`, `"say" """hi"""`},
		{"empty query", "", `""`},
		{"colon", "key:value", `"key:value"`},
		{"parens", "(grouped)", `"(grouped)"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeFTSQuery(c.in); got != c.want {
				t.Fatalf("sanitizeFTSQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSearchHandlesHyphenatedQueries is the regression test for the actual
// bug found: searching for "claude-mem" — arguably the single most likely
// real query against this project — failed FTS5's query parser outright
// ("no such column: mem") before sanitizeFTSQuery existed.
func TestSearchHandlesHyphenatedQueries(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	o := Observation{Type: "discovery", Title: "claude-mem installation found"}
	if _, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "b"), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.Search("", "claude-mem", 10)
	if err != nil {
		t.Fatalf("Search(\"claude-mem\") returned an error instead of results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(\"claude-mem\") returned %d results, want 1", len(results))
	}
}

// TestByIDsFetchesExactRowsAndOmitsUnknownIDs is the get_observations MCP
// tool's real read path: given a set of IDs (some genuine, one not), it
// must return exactly the genuine rows with full detail intact (narrative,
// facts — fields formatSearchResults/RecentByProject's own list-shaped
// callers deliberately don't need, but a detail lookup does) and silently
// drop the unknown one rather than erroring.
func TestByIDsFetchesExactRowsAndOmitsUnknownIDs(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	o1 := Observation{Type: "discovery", Title: "first", Narrative: "narrative one", Facts: []string{"fact a"}}
	r1, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"), o1, 0)
	if err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	o2 := Observation{Type: "discovery", Title: "second", Narrative: "narrative two"}
	r2, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "b", "2"), o2, 0)
	if err != nil {
		t.Fatalf("Insert 2: %v", err)
	}

	results, err := st.ByIDs([]int64{r1.ID, r2.ID, 999999})
	if err != nil {
		t.Fatalf("ByIDs: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ByIDs returned %d results, want 2 (the unknown id 999999 should be silently omitted)", len(results))
	}
	byID := map[int64]SearchResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if got := byID[r1.ID].Observation.Facts; len(got) != 1 || got[0] != "fact a" {
		t.Errorf("ByIDs facts for id=%d = %v, want [\"fact a\"] — full detail must round-trip, not just the abbreviated list fields", r1.ID, got)
	}
	if got := byID[r2.ID].Observation.Narrative; got != "narrative two" {
		t.Errorf("ByIDs narrative for id=%d = %q, want %q", r2.ID, got, "narrative two")
	}
}

func TestByIDsWithEmptySliceReturnsNoRowsNoError(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	results, err := st.ByIDs(nil)
	if err != nil {
		t.Fatalf("ByIDs(nil): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("ByIDs(nil) returned %d results, want 0", len(results))
	}
}
