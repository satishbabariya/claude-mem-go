package sqlite

import (
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

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

	o := memory.Observation{Type: "discovery", Title: "claude-mem installation found"}
	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "b"), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.Search("", "claude-mem", "", 10, 0, 0, 0, "")
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

	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), memory.Observation{Type: "discovery", Title: "widget rollout"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "b", "2"), memory.Observation{Type: "decision", Title: "widget rollout plan approved"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	all, err := st.Search("proj", "widget", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search with no type filter: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("Search with no type filter returned %d results, want 2 (sanity check before filtering)", len(all))
	}

	discoveries, err := st.Search("proj", "widget", "discovery", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search(type=discovery): %v", err)
	}
	if len(discoveries) != 1 || discoveries[0].Observation.Type != "discovery" {
		t.Fatalf("Search(type=discovery) = %+v, want exactly the one discovery-type row", discoveries)
	}

	decisions, err := st.Search("proj", "widget", "decision", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search(type=decision): %v", err)
	}
	if len(decisions) != 1 || decisions[0].Observation.Type != "decision" {
		t.Fatalf("Search(type=decision) = %+v, want exactly the one decision-type row", decisions)
	}

	none, err := st.Search("proj", "widget", "bugfix", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search(type=bugfix): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Search(type=bugfix) = %+v, want 0 (neither seeded row is that type)", none)
	}
}

// TestSearchFiltersByCommaSeparatedObservationTypes is the regression
// test for a real gap: real claude-mem's own search tool documents
// obs_type as "Comma-separated for multiple" (SearchManager.ts splits on
// comma, SessionSearch.ts then builds a type IN (...) clause instead of
// a plain equality one) — this port only ever matched a single type
// exactly, with no split/IN path at all. Seeds three different types and
// confirms a comma-separated filter returns exactly the union of the
// named types, not just accepting the argument as an opaque single
// string that happens to match nothing.
func TestSearchFiltersByCommaSeparatedObservationTypes(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), memory.Observation{Type: "discovery", Title: "gizmo rollout"}, 0); err != nil {
		t.Fatalf("Insert discovery: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "b", "2"), memory.Observation{Type: "decision", Title: "gizmo rollout plan approved"}, 0); err != nil {
		t.Fatalf("Insert decision: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "c", "3"), memory.Observation{Type: "manual", Title: "gizmo rollout manual note"}, 0); err != nil {
		t.Fatalf("Insert manual: %v", err)
	}

	got, err := st.Search("proj", "gizmo", "discovery,decision", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search(type=\"discovery,decision\"): %v", err)
	}
	types := map[string]bool{}
	for _, r := range got {
		types[r.Observation.Type] = true
	}
	if len(got) != 2 || !types["discovery"] || !types["decision"] || types["manual"] {
		t.Fatalf("Search(type=\"discovery,decision\") = %+v, want exactly the discovery and decision rows, not manual", got)
	}

	// Whitespace around commas must be tolerated, matching SearchManager.ts's
	// own .trim() on each split part.
	spaced, err := st.Search("proj", "gizmo", "discovery, decision", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search with spaced comma list: %v", err)
	}
	if len(spaced) != 2 {
		t.Fatalf("Search(type=\"discovery, decision\") = %+v, want 2 (whitespace around commas should be trimmed)", spaced)
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
			memory.ContentHash("s1", "Bash", "page", string(rune('a'+i))),
			memory.Observation{Type: "discovery", Title: "paginated widget rollout"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		seededIDs = append(seededIDs, res.ID)
	}

	page1, err := st.Search("proj", "widget", "", 2, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search page 1: %v", err)
	}
	page2, err := st.Search("proj", "widget", "", 2, 2, 0, 0, "")
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}
	page3, err := st.Search("proj", "widget", "", 2, 4, 0, 0, "")
	if err != nil {
		t.Fatalf("Search page 3: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 {
		t.Fatalf("page sizes = %d, %d, %d, want 2, 2, 1 (5 rows paged 2 at a time)", len(page1), len(page2), len(page3))
	}

	seen := map[int64]int{}
	for _, page := range [][]memory.SearchResult{page1, page2, page3} {
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

// TestSearchFiltersByDateRange is the regression test for a real gap:
// this project's own README excused skipping real claude-mem's
// dateStart/dateEnd search filters with a rationale ("doesn't map onto
// an existing column without a schema redesign") that was false — the
// exact same shape of false excuse the offset test above already caught
// for pagination. created_at_epoch already exists and is already
// indexed (RecentByProject already queries it); this only needed a plain
// WHERE clause. Seeds 3 rows with distinct, directly-set timestamps (not
// Insert's own time.Now(), for exact control) and confirms dateStart/
// dateEnd narrow results the same way real claude-mem's
// SessionSearch.ts's identical >=/<= clauses do.
func TestSearchFiltersByDateRange(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	day := int64(24 * 60 * 60 * 1000)
	base := int64(1700000000000) // an arbitrary but fixed reference point
	var ids [3]int64
	for i := 0; i < 3; i++ {
		res, err := st.Insert("s1", "proj", "Bash",
			memory.ContentHash("s1", "Bash", "daterange", string(rune('a'+i))),
			memory.Observation{Type: "discovery", Title: "dateranged gadget observation"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		ids[i] = res.ID
		epoch := base + int64(i)*day
		if _, err := st.db.Exec(`UPDATE observations SET created_at_epoch = ? WHERE id = ?`, epoch, res.ID); err != nil {
			t.Fatalf("backdating row %d: %v", i, err)
		}
	}

	// dateStart excludes day 0, keeps days 1 and 2.
	fromDay1, err := st.Search("proj", "gadget", "", 10, 0, base+day, 0, "")
	if err != nil {
		t.Fatalf("Search with dateStart: %v", err)
	}
	if got := idSet(fromDay1); !got[ids[1]] || !got[ids[2]] || got[ids[0]] {
		t.Fatalf("Search(dateStart=day1) returned ids %v, want day1 and day2 only (not day0)", got)
	}

	// dateEnd excludes day 2, keeps days 0 and 1.
	toDay1, err := st.Search("proj", "gadget", "", 10, 0, 0, base+day, "")
	if err != nil {
		t.Fatalf("Search with dateEnd: %v", err)
	}
	if got := idSet(toDay1); !got[ids[0]] || !got[ids[1]] || got[ids[2]] {
		t.Fatalf("Search(dateEnd=day1) returned ids %v, want day0 and day1 only (not day2)", got)
	}

	// Both bounds together isolate exactly day 1.
	onlyDay1, err := st.Search("proj", "gadget", "", 10, 0, base+day, base+day, "")
	if err != nil {
		t.Fatalf("Search with both bounds: %v", err)
	}
	if len(onlyDay1) != 1 || onlyDay1[0].ID != ids[1] {
		t.Fatalf("Search(dateStart=dateEnd=day1) = %+v, want exactly [day1]", onlyDay1)
	}
}

func idSet(results []memory.SearchResult) map[int64]bool {
	m := make(map[int64]bool, len(results))
	for _, r := range results {
		m[r.ID] = true
	}
	return m
}

// TestSearchOrderBy locks in real claude-mem's own
// SessionSearch.buildOrderClause semantics: "date_desc"/"date_asc" sort
// by created_at_epoch regardless of text-match rank, and any other,
// unrecognized value falls back to date_desc rather than silently being
// treated as "relevance" — the exact fallback buildOrderClause's own
// default case implements.
func TestSearchOrderBy(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	day := int64(24 * 60 * 60 * 1000)
	base := int64(1700000000000)
	var ids [3]int64
	for i := 0; i < 3; i++ {
		res, err := st.Insert("s1", "proj", "Bash",
			memory.ContentHash("s1", "Bash", "orderby", string(rune('a'+i))),
			memory.Observation{Type: "discovery", Title: "orderby flavored widget"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		ids[i] = res.ID
		if _, err := st.db.Exec(`UPDATE observations SET created_at_epoch = ? WHERE id = ?`,
			base+int64(i)*day, res.ID); err != nil {
			t.Fatalf("backdating row %d: %v", i, err)
		}
	}

	desc, err := st.Search("proj", "widget", "", 10, 0, 0, 0, "date_desc")
	if err != nil {
		t.Fatalf("Search date_desc: %v", err)
	}
	if len(desc) != 3 || desc[0].ID != ids[2] || desc[1].ID != ids[1] || desc[2].ID != ids[0] {
		t.Fatalf("Search(orderBy=date_desc) ids = %v, want newest-first [%d,%d,%d]", idList(desc), ids[2], ids[1], ids[0])
	}

	asc, err := st.Search("proj", "widget", "", 10, 0, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("Search date_asc: %v", err)
	}
	if len(asc) != 3 || asc[0].ID != ids[0] || asc[1].ID != ids[1] || asc[2].ID != ids[2] {
		t.Fatalf("Search(orderBy=date_asc) ids = %v, want oldest-first [%d,%d,%d]", idList(asc), ids[0], ids[1], ids[2])
	}

	// Real claude-mem's own buildOrderClause treats any unrecognized
	// orderBy value the same as date_desc, not as "relevance" — ported
	// deliberately, not an accidental catch-all.
	garbage, err := st.Search("proj", "widget", "", 10, 0, 0, 0, "banana")
	if err != nil {
		t.Fatalf("Search with unrecognized orderBy: %v", err)
	}
	if len(garbage) != 3 || garbage[0].ID != ids[2] || garbage[1].ID != ids[1] || garbage[2].ID != ids[0] {
		t.Fatalf("Search(orderBy=\"banana\") ids = %v, want the same as date_desc [%d,%d,%d]", idList(garbage), ids[2], ids[1], ids[0])
	}
}

func idList(results []memory.SearchResult) []int64 {
	out := make([]int64, len(results))
	for i, r := range results {
		out[i] = r.ID
	}
	return out
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

	o1 := memory.Observation{Type: "discovery", Title: "first", Narrative: "narrative one", Facts: []string{"fact a"}}
	r1, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"), o1, 0)
	if err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	o2 := memory.Observation{Type: "discovery", Title: "second", Narrative: "narrative two"}
	r2, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "b", "2"), o2, 0)
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
	byID := map[int64]memory.SearchResult{}
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
// anticipated in advance: before memory.MaxIDsPerLookup existed, a large enough
// ids slice failed with a raw driver error ("SQL logic error: too many SQL
// variables") instead of a clean, bounded response — the hand-built
// IN (?,?,...) placeholder list has no cap of its own. Confirmed
// empirically that 100,000 IDs triggers it; this test uses a slice just
// over memory.MaxIDsPerLookup itself so it stays fast and doesn't depend on
// exactly where the driver's own real limit sits.
func TestByIDsRejectsTooManyIDs(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	tooMany := make([]int64, memory.MaxIDsPerLookup+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if _, err := st.ByIDs(tooMany); err == nil {
		t.Fatalf("ByIDs with %d ids (limit is %d): want an error, got nil", len(tooMany), memory.MaxIDsPerLookup)
	}

	exactly := tooMany[:memory.MaxIDsPerLookup]
	if _, err := st.ByIDs(exactly); err != nil {
		t.Errorf("ByIDs with exactly %d ids (at the limit): want success, got %v", len(exactly), err)
	}
}

// TestSearchCoversFactsAndConcepts is the SQLite half of a cross-backend
// parity contract. This behavior was always correct here — the FTS5 table
// has covered all five columns since it was created — but it was never
// asserted, which is exactly how the Postgres backend was able to drift
// away from it unnoticed (its search_vector covered only three columns,
// making the same observation findable through one store.Backend
// implementation and invisible through the other). Locking it in on both
// sides means a future change to either can't silently reintroduce the
// divergence. Mirrors TestPostgresSearchCoversFactsAndConcepts exactly,
// same seeded terms.
func TestSearchCoversFactsAndConcepts(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// The distinctive terms live ONLY in facts/concepts — nothing in
	// title/subtitle/narrative mentions them.
	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "facts", "1"),
		memory.Observation{Type: "discovery", Title: "unremarkable heading",
			Facts: []string{"the zorblatt subsystem was replaced"}}, 0); err != nil {
		t.Fatalf("Insert facts row: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "concepts", "2"),
		memory.Observation{Type: "discovery", Title: "another plain heading",
			Concepts: []string{"quibblesnort architecture"}}, 0); err != nil {
		t.Fatalf("Insert concepts row: %v", err)
	}

	factHits, err := st.Search("proj", "zorblatt", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search for a facts-only term: %v", err)
	}
	if len(factHits) != 1 {
		t.Fatalf("Search(\"zorblatt\") returned %d rows, want 1 — the term exists only in facts, which the FTS5 table must cover", len(factHits))
	}

	conceptHits, err := st.Search("proj", "quibblesnort", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search for a concepts-only term: %v", err)
	}
	if len(conceptHits) != 1 {
		t.Fatalf("Search(\"quibblesnort\") returned %d rows, want 1 — the term exists only in concepts, which the FTS5 table must cover", len(conceptHits))
	}
}
