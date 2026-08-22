package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/store"
)

// testDSNEnvVar must be set explicitly for this package's tests to run.
// There is deliberately NO fallback.
//
// There used to be one, and it pointed at
// postgres://claudemem:claudemem@localhost:55432/claudemem — byte-identical
// to the DSN this project's own README documents and docker-compose.yml
// provisions for real use. Every test in this package seeds rows via
// uniqueProject() and none of them delete what they insert, so running
// `go test ./postgres/...` wrote synthetic observations straight into
// whatever store the developer actually uses. Measured on the machine this
// was found on: 7,217 of 7,275 rows (99.2%) were test debris spread across
// 3,968 synthetic projects, leaving 58 real ones. The cost isn't only
// clutter — `reembed` counted 6,064 rows needing work, of which 35 were
// real, so a single remediation run would have spent minutes of Ollama time
// re-embedding garbage.
//
// Requiring the variable is what real claude-mem does for the same reason
// (its own Postgres tests read CLAUDE_MEM_TEST_POSTGRES_URL and skip when
// it's absent, with no default). A skipped test is a visible, recoverable
// state; silently writing into someone's real store is not.
const testDSNEnvVar = "CLAUDE_MEM_GO_TEST_POSTGRES_DSN"

func testDSN() string { return os.Getenv(testDSNEnvVar) }

// requireTestDSN returns the test DSN or skips. Every entry point into
// this package's Postgres tests goes through it — including the few that
// build their own Store instead of using openTestStore — so there is
// exactly one place that can decide to run against a real database.
// nearMissDSNEnvVars are names close enough to testDSNEnvVar that setting
// one is obviously an attempt to run these tests, not a coincidence.
//
// This exists because the failure it prevents actually happened, for a
// long time, unnoticed: CLAUDE_MEM_TEST_POSTGRES_DSN was exported instead
// of CLAUDE_MEM_GO_TEST_POSTGRES_DSN — one missing "GO" — and every local
// run printed
//
//	ok  	claude-mem-go/postgres	5.145s
//
// while 57 of 68 tests skipped and only 11 ran. `go test` prints "ok" for
// a package whose tests all skip, and the skip reason only shows under
// -v, so the output looked like a passing Postgres suite for weeks of
// local verification. CI had the right name, so the tests were genuinely
// running there — but every local "verified against real Postgres" claim
// was weaker than it appeared.
//
// CLAUDE_MEM_TEST_POSTGRES_URL is on the list because it is what real
// claude-mem's own Postgres tests read, which makes it the single most
// likely thing for someone moving between the two codebases to export.
//
// Skipping is the right default for an ABSENT variable — this backend
// needs Docker and shouldn't break `go test ./...` for someone who hasn't
// started it. It is the wrong default for someone who plainly meant to
// run these tests and typo'd the name.
var nearMissDSNEnvVars = []string{
	"CLAUDE_MEM_TEST_POSTGRES_DSN",
	"CLAUDE_MEM_TEST_POSTGRES_URL",
	"CLAUDE_MEM_GO_TEST_POSTGRES_URL",
	"CLAUDE_MEM_POSTGRES_DSN",
	"CLAUDE_MEM_GO_POSTGRES_DSN",
	"POSTGRES_DSN",
}

// nearMissDSNSet returns the first near-miss variable that is set, or ""
// when none is. Split out from requireTestDSN so the detection itself is
// testable without a t.Fatalf that would abort the test asserting it.
func nearMissDSNSet() string {
	if os.Getenv(testDSNEnvVar) != "" {
		return ""
	}
	for _, wrong := range nearMissDSNEnvVars {
		if os.Getenv(wrong) != "" {
			return wrong
		}
	}
	return ""
}

func requireTestDSN(t *testing.T) string {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		if wrong := nearMissDSNSet(); wrong != "" {
			// Fatal, not Skip: this person is trying to run these tests,
			// and silently skipping is how a whole suite reports "ok"
			// while running almost none of itself.
			t.Fatalf("%s is set, but these tests read %s — did you mean %s?\n"+
				"Nothing ran against Postgres. Re-run with:\n"+
				"  %s=$%s go test ./postgres/...",
				wrong, testDSNEnvVar, testDSNEnvVar, testDSNEnvVar, wrong)
		}
		t.Skipf("%s is not set — these tests write real rows, so they will not guess at a database. "+
			"Point it at a THROWAWAY store, never the one you actually use, e.g.\n"+
			"  docker compose up -d && docker exec claude-mem-go-postgres-1 psql -U claudemem -d claudemem -c 'CREATE DATABASE claudemem_test'\n"+
			"  %s=postgres://claudemem:claudemem@localhost:55432/claudemem_test?sslmode=disable go test ./postgres/...",
			testDSNEnvVar, testDSNEnvVar)
	}
	return dsn
}

// openTestStore skips (not fails) the test when Postgres isn't reachable —
// this backend needs Docker running, unlike every other package in this
// project, and a missing dependency should skip cleanly rather than break
// `go test ./...` for someone who hasn't started it.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := requireTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn, DefaultEmbedDims, 0)
	if err != nil {
		t.Skipf("postgres not reachable at %s (start it with `docker compose up -d`): %v", store.RedactDSN(dsn), err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// uniqueProject avoids collisions between test runs against the same
// long-lived database — this isn't a fresh temp file like the SQLite tests
// get, it's a shared Postgres instance.
// uniqueProjectSeq makes uniqueProject unique BY CONSTRUCTION rather than
// by hoping the clock ticked between two calls.
//
// The clock alone was not enough, and this was not theoretical: a
// full-suite run failed in TestPostgresSearchFiltersByObservationTypeWith
// ProjectScope with both its "different" projects carrying the identical
// name, so all three of its rows landed in one project and a
// single-result assertion saw two. Measured on this machine afterwards:
// two adjacent time.Now().UnixNano() reads are identical 92% of the time,
// and two adjacent uniqueProject calls collide 74% of the time. Isolated
// -run invocations happen to pass because the surrounding work spreads
// the calls out; a loaded full-suite run is exactly when it does not,
// which is the worst possible failure schedule for a CI flake.
var uniqueProjectSeq atomic.Int64

func uniqueProject(t *testing.T) string {
	return fmt.Sprintf("test-%s-%d-%d", t.Name(), time.Now().UnixNano(), uniqueProjectSeq.Add(1))
}

func TestPostgresInsertIsIdempotentOnContentHash(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	// Postgres here is a persistent, shared instance (unlike the SQLite
	// tests' fresh temp file per run) — the hash must be unique to THIS run,
	// or re-running the suite collides with a previous run's row and this
	// test would see a false "duplicate" on its very first Insert.
	hash := store.ContentHash("s1", "Bash", "npm test", project)
	o := store.Observation{Type: "discovery", Title: "Tests passed"}

	first, err := st.Insert("s1", project, "Bash", hash, o, 0.01)
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first Insert of a fresh content_hash: want Inserted=true")
	}

	second, err := st.Insert("s1", project, "Bash", hash, o, 0.01)
	if err != nil {
		t.Fatalf("second Insert (duplicate): %v", err)
	}
	if second.Inserted {
		t.Fatal("second Insert with the same content_hash: want Inserted=false")
	}
	if second.ID != first.ID {
		t.Fatalf("second Insert returned ID %d, want %d", second.ID, first.ID)
	}

	count, err := st.CountByProject(project)
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject = %d, want 1", count)
	}
}

// TestPostgresInsertRejectsAnUnrecognizedObservationType is the
// regression test, against the real live container, for a real
// schema-completeness gap found by hand: nothing anywhere validated
// Observation.Type before this — an LLM's <type> tag drifting to an
// unrecognized value would have silently persisted, invisible to any
// -type/type filter with no error anywhere.
func TestPostgresInsertRejectsAnUnrecognizedObservationType(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	_, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "bad-type", project),
		store.Observation{Type: "bugfix", Title: "x"}, 0)
	if err == nil {
		t.Fatal("Insert with an unrecognized type: want an error, got nil")
	}

	count, cerr := st.CountByProject(project)
	if cerr != nil {
		t.Fatalf("CountByProject: %v", cerr)
	}
	if count != 0 {
		t.Fatalf("CountByProject = %d, want 0 — the rejected row must not have been persisted", count)
	}
}

func TestPostgresSearchHandlesHyphenatedQueries(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	o := store.Observation{Type: "discovery", Title: "claude-mem installation found"}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The exact real bug found in the SQLite/FTS5 backend: "claude-mem"
	// broke FTS5's grammar outright. Postgres's plainto_tsquery must not
	// have the same problem — that's the whole reason this backend exists.
	//
	// Scoped to this test's own project: this is a persistent, shared
	// database across every test run ever executed against it, and title
	// "claude-mem installation found" is seeded repeatedly across runs — an
	// unscoped search plus a small LIMIT can genuinely miss this run's own
	// row under accumulated history. Real callers hit the identical
	// scoping requirement for the identical reason (see Search's doc
	// comment), so this isn't a test-only workaround.
	results, err := st.Search(project, "claude-mem", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search(\"claude-mem\") returned an error: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Project == project {
			found = true
		}
	}
	if !found {
		t.Fatalf("Search(\"claude-mem\") did not find the seeded row in project %s among %d results", project, len(results))
	}
}

// TestPostgresSearchFiltersByObservationTypeWithProjectScope exercises
// project and obsType together deliberately — the exact combination the
// old hardcoded `$3` placeholder numbering couldn't have handled if a
// third filter were ever added the same way; this backend now builds
// placeholder numbers dynamically as each optional filter is appended,
// specifically to avoid that class of mistake. Confirms both filters
// apply correctly at once, not just each in isolation.
func TestPostgresSearchFiltersByObservationTypeWithProjectScope(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	otherProject := uniqueProject(t)

	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project), store.Observation{Type: "discovery", Title: "gadget rollout"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project), store.Observation{Type: "decision", Title: "gadget rollout plan approved"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", otherProject, "Bash", store.ContentHash("s1", "Bash", "c", otherProject), store.Observation{Type: "discovery", Title: "gadget rollout"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.Search(project, "gadget", "discovery", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search(project=%s, type=discovery): %v", project, err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(project=%s, type=discovery) = %+v, want exactly 1 (this project's discovery row, not the other project's or the decision row)", project, results)
	}
	if results[0].Project != project || results[0].Observation.Type != "discovery" {
		t.Fatalf("Search(project=%s, type=discovery) returned %+v, want project=%s type=discovery", project, results[0], project)
	}
}

// TestPostgresSearchFiltersByCommaSeparatedObservationTypes mirrors the
// SQLite backend's identical test — the real, live-container regression
// test for a real gap: real claude-mem's own search tool documents
// obs_type as "Comma-separated for multiple," which this backend (like
// the SQLite one) had no split/IN path for at all.
func TestPostgresSearchFiltersByCommaSeparatedObservationTypes(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project), store.Observation{Type: "discovery", Title: "gizmo rollout"}, 0); err != nil {
		t.Fatalf("Insert discovery: %v", err)
	}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project), store.Observation{Type: "decision", Title: "gizmo rollout plan approved"}, 0); err != nil {
		t.Fatalf("Insert decision: %v", err)
	}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "c", project), store.Observation{Type: "manual", Title: "gizmo rollout manual note"}, 0); err != nil {
		t.Fatalf("Insert manual: %v", err)
	}

	got, err := st.Search(project, "gizmo", "discovery,decision", 10, 0, 0, 0, "")
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
}

func TestPostgresSearchRankingAndNoMatch(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	o := store.Observation{Type: "discovery", Title: "A totally unrelated observation about kites"}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.Search(project, "xyzzy_no_such_term_anywhere", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if r.Project == project {
			t.Fatalf("Search for an unrelated term matched project %s — false positive", project)
		}
	}
}

// TestPostgresSearchOffsetPagesWithoutOverlapOrGap is the real,
// live-container regression test for a real gap: offset was skipped on a
// rationale (README: "doesn't map directly onto an existing column")
// that never actually applied to offset itself — it needs no column at
// all, just LIMIT/OFFSET on the existing query. Same scenario as the
// SQLite backend's identical test, independently necessary here since
// this backend's own ORDER BY (ts_rank_cd, then id) is a separate query
// this session added its own tiebreaker to. Seeds 5 rows that all match
// the same term, confirms 3 pages of size 2/2/1 are disjoint and
// together cover every seeded row exactly once.
func TestPostgresSearchOffsetPagesWithoutOverlapOrGap(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	var seededIDs []int64
	for i := 0; i < 5; i++ {
		res, err := st.Insert("s1", project, "Bash",
			store.ContentHash("s1", "Bash", "page", fmt.Sprintf("%s-%d", project, i)),
			store.Observation{Type: "discovery", Title: "paginated widget rollout"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		seededIDs = append(seededIDs, res.ID)
	}

	page1, err := st.Search(project, "widget", "", 2, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search page 1: %v", err)
	}
	page2, err := st.Search(project, "widget", "", 2, 2, 0, 0, "")
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}
	page3, err := st.Search(project, "widget", "", 2, 4, 0, 0, "")
	if err != nil {
		t.Fatalf("Search page 3: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 {
		t.Fatalf("page sizes = %d, %d, %d, want 2, 2, 1 (5 rows paged 2 at a time)", len(page1), len(page2), len(page3))
	}

	seen := map[int64]int{}
	for _, page := range [][]store.SearchResult{page1, page2, page3} {
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

// TestPostgresSearchFiltersByDateRange mirrors the SQLite backend's
// identical test — the real, live-container regression test for the
// same false "doesn't map onto an existing column" excuse this
// project's README used to justify skipping real claude-mem's
// dateStart/dateEnd search filters. created_at_epoch already exists and
// is already indexed on this backend too (idx_observations_created);
// this only needed a plain WHERE clause.
func TestPostgresSearchFiltersByDateRange(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	day := int64(24 * 60 * 60 * 1000)
	base := int64(1700000000000)
	var ids [3]int64
	for i := 0; i < 3; i++ {
		res, err := st.Insert("s1", project, "Bash",
			store.ContentHash("s1", "Bash", "daterange", fmt.Sprintf("%s-%d", project, i)),
			store.Observation{Type: "discovery", Title: "dateranged gadget observation"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		ids[i] = res.ID
		epoch := base + int64(i)*day
		if _, err := st.db.Exec(`UPDATE observations SET created_at_epoch = $1 WHERE id = $2`, epoch, res.ID); err != nil {
			t.Fatalf("backdating row %d: %v", i, err)
		}
	}

	fromDay1, err := st.Search(project, "gadget", "", 10, 0, base+day, 0, "")
	if err != nil {
		t.Fatalf("Search with dateStart: %v", err)
	}
	if got := pgIDSet(fromDay1); !got[ids[1]] || !got[ids[2]] || got[ids[0]] {
		t.Fatalf("Search(dateStart=day1) returned ids %v, want day1 and day2 only (not day0)", got)
	}

	toDay1, err := st.Search(project, "gadget", "", 10, 0, 0, base+day, "")
	if err != nil {
		t.Fatalf("Search with dateEnd: %v", err)
	}
	if got := pgIDSet(toDay1); !got[ids[0]] || !got[ids[1]] || got[ids[2]] {
		t.Fatalf("Search(dateEnd=day1) returned ids %v, want day0 and day1 only (not day2)", got)
	}

	onlyDay1, err := st.Search(project, "gadget", "", 10, 0, base+day, base+day, "")
	if err != nil {
		t.Fatalf("Search with both bounds: %v", err)
	}
	if len(onlyDay1) != 1 || onlyDay1[0].ID != ids[1] {
		t.Fatalf("Search(dateStart=dateEnd=day1) = %+v, want exactly [day1]", onlyDay1)
	}
}

func pgIDSet(results []store.SearchResult) map[int64]bool {
	m := make(map[int64]bool, len(results))
	for _, r := range results {
		m[r.ID] = true
	}
	return m
}

// TestPostgresSearchOrderBy mirrors the SQLite backend's identical test
// against this backend's own ts_rank_cd-based relevance ordering —
// confirms date_desc/date_asc sort by created_at_epoch regardless of
// text-match rank, and that an unrecognized orderBy value falls back to
// date_desc, matching real claude-mem's own buildOrderClause default
// case.
func TestPostgresSearchOrderBy(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	day := int64(24 * 60 * 60 * 1000)
	base := int64(1700000000000)
	var ids [3]int64
	for i := 0; i < 3; i++ {
		res, err := st.Insert("s1", project, "Bash",
			store.ContentHash("s1", "Bash", "orderby", fmt.Sprintf("%s-%d", project, i)),
			store.Observation{Type: "discovery", Title: "orderby flavored widget"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		ids[i] = res.ID
		if _, err := st.db.Exec(`UPDATE observations SET created_at_epoch = $1 WHERE id = $2`,
			base+int64(i)*day, res.ID); err != nil {
			t.Fatalf("backdating row %d: %v", i, err)
		}
	}

	desc, err := st.Search(project, "widget", "", 10, 0, 0, 0, "date_desc")
	if err != nil {
		t.Fatalf("Search date_desc: %v", err)
	}
	if len(desc) != 3 || desc[0].ID != ids[2] || desc[1].ID != ids[1] || desc[2].ID != ids[0] {
		t.Fatalf("Search(orderBy=date_desc) ids = %v, want newest-first [%d,%d,%d]", pgIDList(desc), ids[2], ids[1], ids[0])
	}

	asc, err := st.Search(project, "widget", "", 10, 0, 0, 0, "date_asc")
	if err != nil {
		t.Fatalf("Search date_asc: %v", err)
	}
	if len(asc) != 3 || asc[0].ID != ids[0] || asc[1].ID != ids[1] || asc[2].ID != ids[2] {
		t.Fatalf("Search(orderBy=date_asc) ids = %v, want oldest-first [%d,%d,%d]", pgIDList(asc), ids[0], ids[1], ids[2])
	}

	garbage, err := st.Search(project, "widget", "", 10, 0, 0, 0, "banana")
	if err != nil {
		t.Fatalf("Search with unrecognized orderBy: %v", err)
	}
	if len(garbage) != 3 || garbage[0].ID != ids[2] || garbage[1].ID != ids[1] || garbage[2].ID != ids[0] {
		t.Fatalf("Search(orderBy=\"banana\") ids = %v, want the same as date_desc [%d,%d,%d]", pgIDList(garbage), ids[2], ids[1], ids[0])
	}
}

func pgIDList(results []store.SearchResult) []int64 {
	out := make([]int64, len(results))
	for i, r := range results {
		out[i] = r.ID
	}
	return out
}

// TestPostgresByIDsFetchesExactRowsAndOmitsUnknownIDs verifies `= ANY($1)`
// with a native Go []int64 arg actually works through pgx's stdlib driver
// (not assumed from docs) — the SQLite backend needed a hand-built
// `IN (?,?,...)` placeholder list for the identical lookup since
// database/sql gives it no equivalent way to bind a whole slice.
func TestPostgresByIDsFetchesExactRowsAndOmitsUnknownIDs(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	r1, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project),
		store.Observation{Type: "discovery", Title: "first", Narrative: "narrative one", Facts: []string{"fact a"}}, 0)
	if err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	r2, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project),
		store.Observation{Type: "discovery", Title: "second", Narrative: "narrative two"}, 0)
	if err != nil {
		t.Fatalf("Insert 2: %v", err)
	}

	results, err := st.ByIDs([]int64{r1.ID, r2.ID, 999999999})
	if err != nil {
		t.Fatalf("ByIDs: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ByIDs returned %d results, want 2 (unknown id 999999999 should be silently omitted)", len(results))
	}
	byID := map[int64]store.SearchResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if got := byID[r1.ID].Observation.Facts; len(got) != 1 || got[0] != "fact a" {
		t.Errorf("ByIDs facts for id=%d = %v, want [\"fact a\"]", r1.ID, got)
	}
	if got := byID[r2.ID].Observation.Narrative; got != "narrative two" {
		t.Errorf("ByIDs narrative for id=%d = %q, want %q", r2.ID, got, "narrative two")
	}
}

// TestPostgresByIDsRejectsTooManyIDs confirms this backend enforces the
// identical store.MaxIDsPerLookup bound the SQLite backend needs for a
// real driver limitation — this backend's `= ANY($1)` has no such
// limitation itself, but the bound still applies here for parity: a
// caller shouldn't see a different effective limit depending on which
// backend happens to be active.
func TestPostgresByIDsRejectsTooManyIDs(t *testing.T) {
	st := openTestStore(t)

	tooMany := make([]int64, store.MaxIDsPerLookup+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if _, err := st.ByIDs(tooMany); err == nil {
		t.Fatalf("ByIDs with %d ids (limit is %d): want an error, got nil", len(tooMany), store.MaxIDsPerLookup)
	}
}

func TestPostgresSemanticSearchOrdersByCosineSimilarity(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	oCat, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "cat", project), store.Observation{Type: "discovery", Title: "about cats"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	oDog, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "dog", project), store.Observation{Type: "discovery", Title: "about dogs"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	dims := make([]float32, DefaultEmbedDims)
	dims[0] = 1
	if err := st.SaveEmbedding(oCat.ID, dims); err != nil {
		t.Fatalf("SaveEmbedding cat: %v", err)
	}
	orth := make([]float32, DefaultEmbedDims)
	orth[1] = 1
	if err := st.SaveEmbedding(oDog.ID, orth); err != nil {
		t.Fatalf("SaveEmbedding dog: %v", err)
	}

	query := make([]float32, DefaultEmbedDims)
	query[0] = 1
	// Scoped to this test's own project: same accumulated-history reasoning
	// as the hyphenated-query test above — an unscoped comparison set only
	// grows every time this suite runs against the shared instance, and a
	// fixed limit=50 would eventually push one of these two rows out of
	// the returned set on ranking ties alone.
	matches, err := st.SemanticSearch(project, query, 50)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}

	var catRank, dogRank = -1, -1
	for i, m := range matches {
		if m.ID == oCat.ID {
			catRank = i
		}
		if m.ID == oDog.ID {
			dogRank = i
		}
	}
	if catRank == -1 || dogRank == -1 {
		t.Fatalf("both seeded observations should appear in results; catRank=%d dogRank=%d", catRank, dogRank)
	}
	if catRank >= dogRank {
		t.Fatalf("the aligned vector (cat, rank %d) should rank ahead of the orthogonal one (dog, rank %d)", catRank, dogRank)
	}
}

// TestPostgresOpenRejectsOutOfRangeHNSWEfSearch is the regression test for
// a real, surprising Postgres/pgvector behavior found by hand while
// building this feature: pgvector's own documented 1..1000 bound on
// hnsw.ef_search is NOT reliably enforced by Postgres itself at query
// time. hnsw.ef_search is a custom GUC pgvector's extension registers,
// and until something in a given backend connection has already touched
// the vector extension, Postgres treats the name as an unchecked
// placeholder — reproduced directly: identical Go code (BeginTx → SET
// LOCAL 1001 → Commit) correctly errored through a connection a prior
// real Insert/SaveEmbedding call had already warmed up, but SILENTLY
// accepted the exact same invalid value on an otherwise-idle fresh
// connection whose first-ever query was that SET LOCAL — no error at
// all. A caller configuring this flag has no reliable way to detect a
// typo/misconfiguration from Postgres's own behavior, since whether it
// gets rejected depends on incidental connection warm-up state, not the
// value itself. Fixed by validating the range in Go at Open time instead
// of ever trusting Postgres to catch it — deterministic regardless of
// connection state, confirmed here by using a value (1001) that this
// exact quirk previously let through silently.
func TestPostgresOpenRejectsOutOfRangeHNSWEfSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, v := range []int{1001, -5} {
		_, err := Open(ctx, requireTestDSN(t), DefaultEmbedDims, v)
		if err == nil {
			t.Fatalf("Open with hnswEfSearch=%d (outside pgvector's 1..1000 range): want an error, got nil", v)
		}
		if !strings.Contains(err.Error(), "ef_search") {
			t.Fatalf("Open error = %q, want it to reference hnsw.ef_search", err.Error())
		}
	}
}

// TestPostgresSemanticSearchHNSWEfSearchAppliesWithoutLeaking is the
// real, live regression test that a valid hnsw.ef_search override is
// exercised through a real SemanticSearch call without breaking normal
// results, and — the part that matters for correctness — that
// hnsw.ef_search is confirmed back at its pre-call value immediately
// afterward on the SAME forced single connection (SetMaxOpenConns(1), so
// this is deterministic rather than merely likely with a multi-connection
// pool). See SemanticSearch's own doc comment for why this needs a
// transaction-scoped SET LOCAL rather than a plain SET against a pooled
// *sql.DB — a plain SET would still pass this test's normal-results check
// but would fail the no-leak check below, since it would persist past its
// own call onto whatever unrelated query the pool next hands that
// connection.
func TestPostgresSemanticSearchHNSWEfSearchAppliesWithoutLeaking(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec := make([]float32, DefaultEmbedDims)
	vec[0] = 1

	tuned, err := Open(ctx, requireTestDSN(t), DefaultEmbedDims, 999)
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	defer tuned.Close()
	tuned.db.SetMaxOpenConns(1) // force the same physical connection for both SHOW checks below

	// hnsw.ef_search is a pgvector-registered GUC that Postgres won't
	// recognize for SHOW until something in this backend has actually
	// touched pgvector — confirmed by hand against the real container:
	// SHOW on an otherwise-idle fresh connection errors with "unrecognized
	// configuration parameter", but succeeds immediately after any real
	// vector operation. Warm it up first so the baseline read below
	// reflects pgvector's real default rather than failing outright.
	if _, err := tuned.db.ExecContext(ctx, "SELECT '[1]'::vector"); err != nil {
		t.Fatalf("warm-up vector query: %v", err)
	}
	var baseline string
	if err := tuned.db.QueryRowContext(ctx, "SHOW hnsw.ef_search").Scan(&baseline); err != nil {
		t.Fatalf("SHOW hnsw.ef_search (baseline): %v", err)
	}

	project2 := uniqueProject(t)
	o2, err := tuned.Insert("s1", project2, "Bash", store.ContentHash("s1", "Bash", "efsearch-valid", project2), store.Observation{Type: "discovery", Title: "y"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := tuned.SaveEmbedding(o2.ID, vec); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}
	matches, err := tuned.SemanticSearch(project2, vec, 10)
	if err != nil {
		t.Fatalf("SemanticSearch with a valid override (999): %v", err)
	}
	if len(matches) != 1 || matches[0].ID != o2.ID {
		t.Fatalf("SemanticSearch with ef_search override returned %v, want exactly one match with id=%d — the override must not break normal search results", matches, o2.ID)
	}

	var after string
	if err := tuned.db.QueryRowContext(ctx, "SHOW hnsw.ef_search").Scan(&after); err != nil {
		t.Fatalf("SHOW hnsw.ef_search (after): %v", err)
	}
	if after != baseline {
		t.Fatalf("hnsw.ef_search after SemanticSearch = %q, want it back at the pre-call value %q — SET LOCAL must not leak past its own transaction onto a pooled connection", after, baseline)
	}
}

func TestPostgresRecentByProjectOrdersNewestFirst(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	older, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "1", project), store.Observation{Type: "discovery", Title: "older"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	newer, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "2", project), store.Observation{Type: "discovery", Title: "newer"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.RecentByProject(project, 10)
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

// TestPostgresRecentByProjectHandlesNullNarrative is the same regression
// test as the SQLite backend's: title/subtitle/narrative are nullable, and
// the scan code must not assume otherwise.
func TestPostgresRecentByProjectHandlesNullNarrative(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	hash := store.ContentHash("s1", "Bash", "null-case", project)

	if _, err := st.db.Exec(
		`INSERT INTO observations (session_id, project, tool_name, type, title, content_hash, created_at_epoch)
		 VALUES ('s1', $1, 'Bash', 'discovery', 'title only, no subtitle or narrative', $2, 1)`,
		project, hash,
	); err != nil {
		t.Fatalf("insert row with NULL narrative/subtitle: %v", err)
	}

	results, err := st.RecentByProject(project, 10)
	if err != nil {
		t.Fatalf("RecentByProject with a NULL narrative row: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Observation.Narrative != "" {
		t.Fatalf("Narrative = %q, want empty string for a NULL column", results[0].Observation.Narrative)
	}
}

func TestPostgresBySessionIDOrdersOldestFirst(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())

	first, err := st.Insert(sessionID, project, "Bash", store.ContentHash(sessionID, "Bash", "1", project), store.Observation{Type: "discovery", Title: "first"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	second, err := st.Insert(sessionID, project, "Bash", store.ContentHash(sessionID, "Bash", "2", project), store.Observation{Type: "discovery", Title: "second"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.BySessionID(sessionID, 10)
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

func TestPostgresObservationsForFileMatchesReadAndModifiedExactly(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	readMatch, err := st.Insert("s1", project, "Read", store.ContentHash("s1", "Read", "1", project),
		store.Observation{Type: "discovery", Title: "read match", FilesRead: []string{"main.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	modifiedMatch, err := st.Insert("s1", project, "Edit", store.ContentHash("s1", "Edit", "2", project),
		store.Observation{Type: "change", Title: "modified match", FilesModified: []string{"main.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert("s1", project, "Read", store.ContentHash("s1", "Read", "3", project),
		store.Observation{Type: "discovery", Title: "substring only", FilesRead: []string{"not-main.go-really"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.ObservationsForFile(project, "main.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ObservationsForFile(\"main.go\") returned %d results, want exactly 2 (read + modified matches, not the substring-only row)", len(results))
	}
	ids := map[int64]bool{results[0].ID: true, results[1].ID: true}
	if !ids[readMatch.ID] || !ids[modifiedMatch.ID] {
		t.Fatalf("results = %v, want both %d and %d", ids, readMatch.ID, modifiedMatch.ID)
	}
}

func TestPostgresSemanticSearchDimensionMismatchErrors(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	o, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "c", project), store.Observation{Type: "discovery", Title: "x"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Wrong dimensionality must fail loudly, not silently truncate/pad —
	// this is the schema's fixed-dims guarantee actually being enforced.
	if err := st.SaveEmbedding(o.ID, []float32{1, 2, 3}); err == nil {
		t.Fatal("SaveEmbedding with the wrong vector dimensionality: want an error, got nil")
	}
}

// TestPostgresOpenRecordsMigrationAndReopenDoesNotReapply is the Postgres
// half of the schema-versioning framework's regression coverage (see
// migrate/migrate_test.go and store/migrations_test.go for the shared
// runner and the SQLite side) — this backend never had ANY migration
// tracking before, just an unconditional idempotent schema block re-run on
// every Open. Confirms schema_migrations actually gets populated against
// a real container, and that reopening doesn't insert a duplicate row.
func TestPostgresOpenRecordsMigrationAndReopenDoesNotReapply(t *testing.T) {
	st := openTestStore(t)

	var count1 int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&count1); err != nil {
		t.Fatalf("count schema_migrations after first Open: %v", err)
	}
	if count1 != 1 {
		t.Fatalf("schema_migrations has %d rows for version 1, want exactly 1", count1)
	}
	st.Close()

	st2, err := Open(context.Background(), requireTestDSN(t), DefaultEmbedDims, 0)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer st2.Close()

	var count2 int
	if err := st2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&count2); err != nil {
		t.Fatalf("count schema_migrations after second Open: %v", err)
	}
	if count2 != 1 {
		t.Fatalf("schema_migrations has %d rows for version 1 after reopening, want 1 (no duplicate from re-applying)", count2)
	}
}

// TestPostgresOpenBoundsConnectionPool confirms Open doesn't leave the
// pool at database/sql's default of unlimited — real risk now that every
// long-lived caller (worker.Daemon, mcpserver.Server) opens one Store for
// its whole process lifetime instead of one per event: an unbounded pool
// is the only thing standing between a burst of concurrent calls and
// Postgres's own max_connections, shared with every other client on the
// same server.
func TestPostgresOpenBoundsConnectionPool(t *testing.T) {
	st := openTestStore(t)
	stats := st.db.Stats()
	if stats.MaxOpenConnections <= 0 {
		t.Fatalf("MaxOpenConnections = %d, want a positive bound, not database/sql's default of unlimited (0)", stats.MaxOpenConnections)
	}
}

// TestPostgresOpenPoolMaxIsConfigurable is the regression test for a real
// gap: MaxOpenConns matched real claude-mem's own DEFAULT_POOL_MAX value
// (10) but, unlike connectionTimeout()/withStatementTimeout(), had no
// env var override at all — an operator sharing one Postgres server
// across many deployments (the exact scenario this pool's own doc
// comment discusses) had no way to tune it, unlike every other knob real
// claude-mem's config.ts exposes.
func TestPostgresOpenPoolMaxIsConfigurable(t *testing.T) {
	t.Setenv(poolMaxEnvVar, "3")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := Open(ctx, requireTestDSN(t), DefaultEmbedDims, 0)
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	defer st.Close()

	if got := st.db.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("MaxOpenConnections = %d with %s=3, want 3", got, poolMaxEnvVar)
	}
}

// TestPostgresOpenIdleTimeoutMatchesRealDefault locks in the real,
// found-by-hand mismatch this fix closes: Open used to hardcode
// SetConnMaxIdleTime to 5 minutes, a 10x mismatch against real
// claude-mem's own DEFAULT_IDLE_TIMEOUT_MS of 30 seconds, with no
// override path at all.
func TestPostgresOpenIdleTimeoutMatchesRealDefault(t *testing.T) {
	if got, want := idleTimeout(), 30*time.Second; got != want {
		t.Fatalf("idleTimeout() with no env override = %s, want %s (real claude-mem's own DEFAULT_IDLE_TIMEOUT_MS)", got, want)
	}

	t.Setenv(idleTimeoutEnvVar, "1234")
	if got, want := idleTimeout(), 1234*time.Millisecond; got != want {
		t.Fatalf("idleTimeout() with %s=1234 = %s, want %s", idleTimeoutEnvVar, got, want)
	}
}

// TestPostgresOpenPoolMaxDefaultsAndEnvOverride mirrors
// TestConnectionTimeoutDefaultsAndEnvOverride for poolMax().
func TestPostgresOpenPoolMaxDefaultsAndEnvOverride(t *testing.T) {
	if got, want := poolMax(), defaultPoolMax; got != want {
		t.Fatalf("poolMax() with no env override = %d, want %d", got, want)
	}

	t.Setenv(poolMaxEnvVar, "7")
	if got, want := poolMax(), 7; got != want {
		t.Fatalf("poolMax() with %s=7 = %d, want %d", poolMaxEnvVar, got, want)
	}

	t.Setenv(poolMaxEnvVar, "not-a-number")
	if got, want := poolMax(), defaultPoolMax; got != want {
		t.Fatalf("poolMax() with a garbage env var = %d, want it to fall back to %d", got, want)
	}
}

func TestSupportsIterativeScanVersionParsing(t *testing.T) {
	// Guards the fallback decision: guessing "supported" on an older
	// pgvector doesn't degrade gracefully, it breaks SemanticSearch
	// outright, so anything uncertain must read as false.
	st := openTestStore(t)
	if !st.iterativeScan {
		t.Skipf("this container's pgvector predates 0.8 — nothing to assert about the fast path")
	}
	var version string
	if err := st.db.QueryRow(`SELECT extversion FROM pg_extension WHERE extname='vector'`).Scan(&version); err != nil {
		t.Fatalf("read pgvector version: %v", err)
	}
	t.Logf("pgvector %s detected as iterative-scan capable", version)
}

// TestPostgresSemanticSearchScopedToProjectReturnsResults is the
// regression test for a real, measured bug: the project predicate is a
// POST-filter on the HNSW scan, so pgvector walked hnsw.ef_search
// globally-nearest candidates and then dropped every one whose project
// didn't match — returning ZERO rows, with no error, for a project that
// genuinely had thousands of embedded observations.
//
// Reproduced by hand at the scale where the planner actually chooses the
// HNSW plan (60,000 embedded rows in the queried project, 20,000 in
// another whose vectors sat nearer the query): the real Go API returned
// 0 matches. This test asserts the correctness and project-isolation
// invariants at a size that runs fast; the plan-dependent at-scale
// reproduction is recorded in the README rather than paid for on every
// test run.
func TestPostgresSemanticSearchScopedToProjectReturnsResults(t *testing.T) {
	st := openTestStore(t)
	target := uniqueProject(t)
	other := uniqueProject(t)

	// `other`'s vectors sit ON the query vector; `target`'s are
	// orthogonal to it — the arrangement that starves a post-filter.
	near := make([]float32, DefaultEmbedDims)
	near[0] = 1
	far := make([]float32, DefaultEmbedDims)
	far[1] = 1

	seed := func(project string, vec []float32, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			res, err := st.Insert("s1", project, "Bash",
				store.ContentHash("s1", "Bash", project, fmt.Sprintf("%d", i)),
				store.Observation{Type: "discovery", Title: fmt.Sprintf("%s row %d", project, i)}, 0)
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			v := append([]float32(nil), vec...)
			v[DefaultEmbedDims-1] = float32(i) / 1e6
			if err := st.SaveEmbedding(res.ID, v); err != nil {
				t.Fatalf("SaveEmbedding: %v", err)
			}
		}
	}
	seed(other, near, 40)
	seed(target, far, 20)

	got, err := st.SemanticSearch(target, near, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("SemanticSearch(%q) returned %d rows, want 10 — the queried project has 20 embedded rows; returning fewer means the project filter starved the candidate set", target, len(got))
	}
	for _, m := range got {
		if m.Project != target {
			t.Fatalf("SemanticSearch(%q) leaked a row from project %q", target, m.Project)
		}
	}
}

// TestPostgresSemanticSearchCTEFallbackPath exercises the older-pgvector
// path directly — the one taken when hnsw.iterative_scan isn't available
// — since the container under test almost certainly supports iterative
// scan and would otherwise never run this code.
func TestPostgresSemanticSearchCTEFallbackPath(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	vec := make([]float32, DefaultEmbedDims)
	vec[0] = 1
	for i := 0; i < 5; i++ {
		res, err := st.Insert("s1", project, "Bash",
			store.ContentHash("s1", "Bash", project, fmt.Sprintf("%d", i)),
			store.Observation{Type: "discovery", Title: fmt.Sprintf("row %d", i)}, 0)
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		v := append([]float32(nil), vec...)
		v[DefaultEmbedDims-1] = float32(i) / 1e6
		if err := st.SaveEmbedding(res.ID, v); err != nil {
			t.Fatalf("SaveEmbedding: %v", err)
		}
	}

	st.iterativeScan = false // force the fallback
	got, err := st.SemanticSearch(project, vec, 10)
	if err != nil {
		t.Fatalf("SemanticSearch via the CTE fallback: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("CTE fallback returned %d rows, want all 5 seeded", len(got))
	}
	for _, m := range got {
		if m.Project != project {
			t.Fatalf("CTE fallback leaked a row from project %q", m.Project)
		}
	}
}

// TestSemanticSearchPlanNeverLeavesAScopedSearchUnguarded is the
// deterministic regression test for the post-filter bug. It asserts the
// routing decision rather than the row count, deliberately: the bug is
// planner-dependent and only appears once the table is large enough for
// Postgres to choose the HNSW plan (measured by hand at 60,000 rows in
// the queried project), so a row-count assertion at test scale passes
// whether or not the guard exists — which is precisely how this survived
// unnoticed. What must hold at every scale is that a scoped search is
// never issued as a bare HNSW post-filter.
func TestSemanticSearchPlanNeverLeavesAScopedSearchUnguarded(t *testing.T) {
	for _, iterative := range []bool{true, false} {
		s := &Store{iterativeScan: iterative}

		useCTE, useIter := s.semanticSearchPlan("some-project")
		if !useCTE && !useIter {
			t.Fatalf("iterativeScan=%v: a SCOPED search got neither the CTE pre-filter nor iterative scan — that is the bare HNSW post-filter that silently returns zero rows", iterative)
		}
		if useCTE && useIter {
			t.Fatalf("iterativeScan=%v: both strategies selected at once", iterative)
		}
		if iterative && !useIter {
			t.Fatal("pgvector supports iterative scan but the slow CTE path was chosen")
		}
		if !iterative && !useCTE {
			t.Fatal("pgvector lacks iterative scan but the CTE fallback was not chosen")
		}

		// Unscoped needs no guard: with no filter there is nothing for a
		// post-filter to discard, and forcing the CTE would throw away
		// the ANN index for no reason.
		if c, i := s.semanticSearchPlan(""); c || i {
			t.Fatalf("iterativeScan=%v: an UNSCOPED search should use the plain HNSW path, got useCTE=%v useIterative=%v", iterative, c, i)
		}
	}
}

// TestPostgresSaveEmbeddingRejectsAnUnknownObservation is a cross-backend
// parity fix: an UPDATE matching no rows is not success. The SQLite
// backend stores embeddings in a separate table with a foreign key, so
// the same call there fails loudly with a constraint violation; this one
// returned nil, telling the caller an embedding was saved when nothing
// was written. Measured divergence, not theoretical.
func TestPostgresSaveEmbeddingRejectsAnUnknownObservation(t *testing.T) {
	st := openTestStore(t)
	vec := make([]float32, st.embedDims)
	err := st.SaveEmbedding(999999999, vec)
	if err == nil {
		t.Fatal("SaveEmbedding for a nonexistent observation returned nil — the caller would believe an embedding was stored when the UPDATE matched no rows")
	}
	if !strings.Contains(err.Error(), "no such observation") {
		t.Fatalf("error = %v, want it to name the missing observation", err)
	}
}

// TestPostgresSaveEmbeddingRejectsWrongDimensionsActionably locks in the
// message, not just the failure. pgvector's own error ("expected 768
// dimensions, not 384") says nothing about WHY the two differ or what to
// do, and the difference is always a configuration mismatch: the column's
// width is fixed when the store is created and can never be widened
// afterwards.
func TestPostgresSaveEmbeddingRejectsWrongDimensionsActionably(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	res, err := st.Insert("s1", project, "Bash",
		store.ContentHash("s1", "Bash", project, "dims"),
		store.Observation{Type: "discovery", Title: "row"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	wrong := make([]float32, st.embedDims+1)
	err = st.SaveEmbedding(res.ID, wrong)
	if err == nil {
		t.Fatal("SaveEmbedding with the wrong dimension count: want an error, got nil")
	}
	for _, want := range []string{"fixed", defaultEmbedDimsEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to mention %q so the operator knows this is a configuration mismatch and how to resolve it", err, want)
		}
	}
}

// TestConfiguredEmbedDims covers the env-var override that makes a
// non-default embedding model usable on a NEW store. Every caller passes
// 0 for embedDims (14 call sites, none plumbing it), so before this the
// Postgres backend was hard-wired to 768 with no override anywhere —
// all-minilm (384) and mxbai-embed-large (1024) simply could not be used.
func TestConfiguredEmbedDims(t *testing.T) {
	if got := configuredEmbedDims(DefaultEmbedDims); got != DefaultEmbedDims {
		t.Fatalf("configuredEmbedDims with no env var = %d, want the %d default", got, DefaultEmbedDims)
	}
	t.Setenv(defaultEmbedDimsEnvVar, "384")
	if got := configuredEmbedDims(DefaultEmbedDims); got != 384 {
		t.Fatalf("configuredEmbedDims with %s=384 = %d, want 384", defaultEmbedDimsEnvVar, got)
	}
	// Anything unusable falls back rather than creating a broken column.
	for _, bad := range []string{"not-a-number", "0", "-5"} {
		t.Setenv(defaultEmbedDimsEnvVar, bad)
		if got := configuredEmbedDims(DefaultEmbedDims); got != DefaultEmbedDims {
			t.Fatalf("configuredEmbedDims with %s=%q = %d, want the %d default", defaultEmbedDimsEnvVar, bad, got, DefaultEmbedDims)
		}
	}
}

// TestColumnEmbedDimsReadsTheRealWidth confirms Open adopts the column's
// ACTUAL width rather than the requested one. They diverge whenever a
// store predates the current configuration — the column is created once
// by migration v1 and CREATE TABLE IF NOT EXISTS can never widen it — and
// every dimension check must be against what the column will really
// accept.
func TestColumnEmbedDimsReadsTheRealWidth(t *testing.T) {
	st := openTestStore(t)
	if st.embedDims <= 0 {
		t.Fatalf("Store.embedDims = %d, want the real column width", st.embedDims)
	}
	var typmod int
	if err := st.db.QueryRow(`SELECT atttypmod FROM pg_attribute WHERE attrelid='observations'::regclass AND attname='embedding'`).Scan(&typmod); err != nil {
		t.Fatalf("read column width: %v", err)
	}
	if st.embedDims != typmod {
		t.Fatalf("Store.embedDims = %d but the column is vector(%d)", st.embedDims, typmod)
	}
}

// TestNearMissDSNDetection guards the guard. The bug it exists for was
// invisible precisely because the failure mode was silence: a package
// whose tests all skip still prints "ok", so 57 skipped tests looked
// exactly like a passing Postgres suite.
func TestNearMissDSNDetection(t *testing.T) {
	t.Run("the exact mistake that was made", func(t *testing.T) {
		t.Setenv(testDSNEnvVar, "")
		t.Setenv("CLAUDE_MEM_TEST_POSTGRES_DSN", "postgres://x/y")
		if got := nearMissDSNSet(); got != "CLAUDE_MEM_TEST_POSTGRES_DSN" {
			t.Errorf("nearMissDSNSet() = %q, want the near-miss name", got)
		}
	})

	// Real claude-mem's own Postgres tests read this name, which makes it
	// the likeliest thing for someone moving between the two codebases to
	// export.
	t.Run("real claude-mem's variable name", func(t *testing.T) {
		t.Setenv(testDSNEnvVar, "")
		t.Setenv("CLAUDE_MEM_TEST_POSTGRES_URL", "postgres://x/y")
		if got := nearMissDSNSet(); got != "CLAUDE_MEM_TEST_POSTGRES_URL" {
			t.Errorf("nearMissDSNSet() = %q, want the near-miss name", got)
		}
	})

	// The correct variable wins outright: a near-miss left over in a shell
	// must not turn a correctly-configured run into a failure.
	t.Run("correct variable set alongside a near-miss", func(t *testing.T) {
		t.Setenv(testDSNEnvVar, "postgres://real/db")
		t.Setenv("CLAUDE_MEM_TEST_POSTGRES_DSN", "postgres://x/y")
		if got := nearMissDSNSet(); got != "" {
			t.Errorf("nearMissDSNSet() = %q, want \"\" — the correct variable is set, so nothing is wrong", got)
		}
	})

	// Nothing set at all is the ordinary "Docker isn't running" case and
	// must stay a clean skip, not a failure.
	t.Run("nothing set is not a near miss", func(t *testing.T) {
		t.Setenv(testDSNEnvVar, "")
		for _, v := range nearMissDSNEnvVars {
			t.Setenv(v, "")
		}
		if got := nearMissDSNSet(); got != "" {
			t.Errorf("nearMissDSNSet() = %q, want \"\" when nothing is set", got)
		}
	})
}
