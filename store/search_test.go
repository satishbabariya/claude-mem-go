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

	results, err := st.Search("", "claude-mem", "", 10, 0)
	if err != nil {
		t.Fatalf("Search(\"claude-mem\") returned an error instead of results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(\"claude-mem\") returned %d results, want 1", len(results))
	}
}

// TestSearchFiltersByObservationType is the real parity gap this closes:
// real claude-mem's own search tool takes an obs_type filter (its docs
// call out "bugfix, feature" as examples); this project's version filters
// against the actual, small, fixed vocabulary the observer itself ever
// writes (discovery/change/decision/summary/manual). Seeds two matching
// titles under different types and confirms the filter actually narrows
// results, not just accepts the argument without effect.
func TestSearchFiltersByObservationType(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"), Observation{Type: "discovery", Title: "widget rollout"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "b", "2"), Observation{Type: "decision", Title: "widget rollout plan approved"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	all, err := st.Search("proj", "widget", "", 10, 0)
	if err != nil {
		t.Fatalf("Search with no type filter: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("Search with no type filter returned %d results, want 2 (sanity check before filtering)", len(all))
	}

	discoveries, err := st.Search("proj", "widget", "discovery", 10, 0)
	if err != nil {
		t.Fatalf("Search(type=discovery): %v", err)
	}
	if len(discoveries) != 1 || discoveries[0].Observation.Type != "discovery" {
		t.Fatalf("Search(type=discovery) = %+v, want exactly the one discovery-type row", discoveries)
	}

	decisions, err := st.Search("proj", "widget", "decision", 10, 0)
	if err != nil {
		t.Fatalf("Search(type=decision): %v", err)
	}
	if len(decisions) != 1 || decisions[0].Observation.Type != "decision" {
		t.Fatalf("Search(type=decision) = %+v, want exactly the one decision-type row", decisions)
	}

	none, err := st.Search("proj", "widget", "bugfix", 10, 0)
	if err != nil {
		t.Fatalf("Search(type=bugfix): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Search(type=bugfix) = %+v, want 0 (neither seeded row is that type)", none)
	}
}

// TestSearchOffsetPagesWithoutOverlapOrGap is the regression test for a
// real gap: offset was skipped on a rationale (README: "doesn't map
// directly onto an existing column") that never actually applied to
// offset itself — it needs no column at all, just LIMIT/OFFSET on the
// existing query. Seeds 5 rows that all match the same term, confirms
// page 1 (limit=2, offset=0) and page 2 (limit=2, offset=2) are disjoint
// and together with page 3 (offset=4) cover every seeded row exactly
// once — not just "offset changes the result," which a broken
// (non-deterministic) ordering could also produce by accident.
func TestSearchOffsetPagesWithoutOverlapOrGap(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var seededIDs []int64
	for i := 0; i < 5; i++ {
		res, err := st.Insert("s1", "proj", "Bash",
			ContentHash("s1", "Bash", "page", string(rune('a'+i))),
			Observation{Type: "discovery", Title: "paginated widget rollout"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		seededIDs = append(seededIDs, res.ID)
	}

	page1, err := st.Search("proj", "widget", "", 2, 0)
	if err != nil {
		t.Fatalf("Search page 1: %v", err)
	}
	page2, err := st.Search("proj", "widget", "", 2, 2)
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}
	page3, err := st.Search("proj", "widget", "", 2, 4)
	if err != nil {
		t.Fatalf("Search page 3: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 {
		t.Fatalf("page sizes = %d, %d, %d, want 2, 2, 1 (5 rows paged 2 at a time)", len(page1), len(page2), len(page3))
	}

	seen := map[int64]int{}
	for _, page := range [][]SearchResult{page1, page2, page3} {
		for _, r := range page {
			seen[r.ID]++
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pages together covered %d distinct rows, want all 5 seeded rows exactly once: %v", len(seen), seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("row id=%d appeared %d times across pages, want exactly once (offset must not overlap between pages)", id, count)
		}
	}
	for _, id := range seededIDs {
		if seen[id] != 1 {
			t.Fatalf("seeded row id=%d missing from paginated results entirely", id)
		}
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

// TestByIDsRejectsTooManyIDs is the regression test for a real bug found
// against this project's own SQLite driver (modernc.org/sqlite), not
// anticipated in advance: before MaxIDsPerLookup existed, a large enough
// ids slice failed with a raw driver error ("SQL logic error: too many SQL
// variables") instead of a clean, bounded response — the hand-built
// IN (?,?,...) placeholder list has no cap of its own. Confirmed
// empirically that 100,000 IDs triggers it; this test uses a slice just
// over MaxIDsPerLookup itself so it stays fast and doesn't depend on
// exactly where the driver's own real limit sits.
func TestByIDsRejectsTooManyIDs(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	tooMany := make([]int64, MaxIDsPerLookup+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if _, err := st.ByIDs(tooMany); err == nil {
		t.Fatalf("ByIDs with %d ids (limit is %d): want an error, got nil", len(tooMany), MaxIDsPerLookup)
	}

	exactly := tooMany[:MaxIDsPerLookup]
	if _, err := st.ByIDs(exactly); err != nil {
		t.Errorf("ByIDs with exactly %d ids (at the limit): want success, got %v", len(exactly), err)
	}
}
