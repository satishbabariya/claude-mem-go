// Full-text search over observations, via SQLite's built-in FTS5 virtual
// table module — mirrors the observations_fts shadow table and
// observations_ai trigger claude-mem's real SessionStore.ts maintains
// (src/services/sqlite/SessionStore.ts's observationsFTSTriggersSQL):
//
//	CREATE TRIGGER observations_ai AFTER INSERT ON observations BEGIN
//	  INSERT INTO observations_fts(rowid, title, subtitle, narrative, text, facts, concepts)
//	  VALUES (new.id, new.title, new.subtitle, new.narrative, new.text, new.facts, new.concepts);
//	END;
//
// This keeps the same shape minus the `text` column (this schema never had
// one — see store.go's doc comment on why the schema is a narrower subset).
//
// Deliberately NOT here: semantic/vector search. The real claude-mem also
// syncs observations into Chroma (a Python vector DB) for embedding-based
// search — that requires an embeddings provider (Voyage/OpenAI/etc., a new
// external dependency and API key this project hasn't needed anywhere else)
// and a vector index, which is a meaningfully bigger addition than "wire up
// a SQLite feature that's already compiled in." FTS5 covers keyword search
// today; semantic search is a real but separate scope decision.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// This is deliberately a SELF-CONTAINED fts5 table (no content=/content_rowid=
// "external content" mode), even though claude-mem's real SessionStore.ts
// does use external content. Verified by hand that with
// modernc.org/sqlite specifically, an external-content fts5 table populated
// through the standard trigger-insert pattern does not become MATCH-able
// until an explicit `INSERT INTO observations_fts(observations_fts) VALUES
// ('rebuild')` — reproduced identically through both the Go driver and a
// small isolated test, while the exact same external-content schema worked
// immediately (no rebuild needed) through the plain `sqlite3` CLI. That
// points at a driver-specific gap in modernc.org/sqlite's external-content
// support, not a mistake in the trigger SQL itself. Self-contained mode
// sidesteps it entirely — small storage duplication in exchange for
// actually working without a rebuild step.
const createFTSSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(
	title, subtitle, narrative, facts, concepts
);
CREATE TRIGGER IF NOT EXISTS observations_ai AFTER INSERT ON observations BEGIN
	INSERT INTO observations_fts(rowid, title, subtitle, narrative, facts, concepts)
	VALUES (new.id, new.title, new.subtitle, new.narrative, new.facts, new.concepts);
END;
-- A plain DELETE, not the fts5 "special command" INSERT INTO
-- observations_fts(observations_fts) VALUES ('delete', rowid, ...col
-- values...) form — that form is for CONTENTLESS or EXTERNAL CONTENT
-- tables, which need the old column values handed back to them because
-- they don't store their own content. This table is neither (see the
-- doc comment above on why it's self-contained); it owns its own rows, so
-- it can delete by rowid directly. A real, previously-latent bug: this
-- trigger used the special-command form for years without ever being
-- exercised, because nothing deleted from observations until Prune
-- (prune.go) — the first real DELETE hit it immediately with a genuine
-- "SQL logic error" from SQLite itself, reproduced via both the Go driver
-- and the plain sqlite3 CLI, confirming it's the trigger SQL, not a
-- driver quirk.
CREATE TRIGGER IF NOT EXISTS observations_ad AFTER DELETE ON observations BEGIN
	DELETE FROM observations_fts WHERE rowid = old.id;
END;
`

// sanitizeFTSQuery turns a plain user query into safe FTS5 MATCH syntax.
//
// Found the hard way, not anticipated: FTS5's query parser treats bareword
// punctuation specially — "claude-mem" (a hyphen, arguably the single most
// likely real query against THIS project) fails outright with "no such
// column: mem", because a bare "-" prefixes a NOT-clause / column filter in
// FTS5's grammar. The same problem applies to any bareword containing `:`,
// `(`, `)`, `"`, or `*`. Quoting each bareword as a phrase sidesteps all of
// that — FTS5 tokenizes quoted content with the same tokenizer used at index
// time, so "claude-mem" and claude-mem still match identically, just without
// the raw text ever reaching the operator grammar.
//
// AND/OR/NOT are preserved unquoted so boolean queries keep working (e.g.
// "monetization OR sqlite" — verified against a real query in this project's
// own testing) — FTS5 only recognizes those three keywords as operators when
// they appear in uppercase and unquoted, so this only intercepts genuine
// boolean usage, not e.g. a search for the word "and".
func sanitizeFTSQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return `""` // an empty phrase matches nothing, rather than erroring on an empty MATCH string
	}
	for i, f := range fields {
		switch f {
		case "AND", "OR", "NOT":
			continue
		default:
			fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
		}
	}
	return strings.Join(fields, " ")
}

func ensureFTS(db *sql.DB) error {
	if _, err := db.Exec(createFTSSQL); err != nil {
		return err
	}
	// Backfill: rows inserted before this table existed (or by a version of
	// this package built before search.go) aren't covered by the AFTER
	// INSERT trigger, which only fires on new inserts going forward. This
	// INSERT...SELECT is idempotent — re-running it on an already-backfilled
	// database only ever matches rows genuinely missing from the index.
	_, err := db.Exec(`
		INSERT INTO observations_fts(rowid, title, subtitle, narrative, facts, concepts)
		SELECT id, title, subtitle, narrative, facts, concepts FROM observations
		WHERE id NOT IN (SELECT rowid FROM observations_fts)
	`)
	return err
}

// nullableTextFields scans title/subtitle/narrative — nullable TEXT columns
// in the schema — into sql.NullString rather than directly into a plain Go
// string. Found the hard way: a hand-inserted test row that omitted
// narrative (leaving it SQL NULL, which the schema permits) failed every
// query that read it with "converting NULL to string is unsupported." The
// normal Insert() path always supplies at least an empty string, so this
// never surfaced through ordinary use — but a nullable column that the scan
// code can't actually handle NULL for is a latent bug regardless of how
// unlikely a real trigger is, and RecentByProject (a brand new query at the
// time this was found) hit it on the very first manually-seeded row.
type nullableTextFields struct {
	title, subtitle, narrative sql.NullString
}

func (n nullableTextFields) apply(o *memory.Observation) {
	o.Title = n.title.String
	o.Subtitle = n.subtitle.String
	o.Narrative = n.narrative.String
}

// clampNegativeLimit guards every LIMIT-bounded query in this file against
// a real SQLite quirk, found the hard way against this project's own
// driver: SQLite's LIMIT treats a negative value as "unlimited," not
// "zero" — the same root cause already fixed for Timeline, applying here
// to every other paginated read path too (Search, RecentByProject,
// BySessionID, ObservationsForFile, ExportAll,
// ObservationsNeedingEmbedding all reproduced returning every row in the
// table when given limit=-1, confirmed by hand against a real seeded
// database, not assumed from Timeline's fix alone). LIMIT 0 already
// behaves correctly (an empty result, confirmed separately) — only
// negative values need clamping. mcpserver.go's own caller already
// substitutes a default before calling any of these, so this is
// defense-in-depth for the memory.Backend contract itself, the same reasoning as
// Timeline's clamp.
func clampNegativeLimit(limit int) int {
	if limit < 0 {
		return 0
	}
	return limit
}

// searchOrderClause mirrors real claude-mem's own
// SessionSearch.buildOrderClause: "relevance" (also the default when
// orderBy is empty) ranks by FTS5's own bare `rank` column, tied against
// id for a fully deterministic order (needed for LIMIT/OFFSET pagination
// to stay disjoint across calls — a rank tie is real, not hypothetical,
// for identical term-frequency shapes). "date_desc"/"date_asc" switch to
// created_at_epoch instead; any other, unrecognized value also falls
// back to date_desc, matching buildOrderClause's own default case
// exactly rather than silently treating it as relevance.
func searchOrderClause(orderBy string, enumerate bool) string {
	switch orderBy {
	case "", "relevance":
		// "Relevance" is meaningless without a query to be relevant TO,
		// and `rank` does not even exist outside an FTS MATCH — asking
		// for it here is a SQL error, not a bad ordering. Newest-first
		// is the sensible default for enumeration and matches what
		// RecentByProject already returns.
		if enumerate {
			return "ORDER BY o.created_at_epoch DESC, o.id DESC"
		}
		return "ORDER BY rank, o.id"
	case "date_asc":
		return "ORDER BY o.created_at_epoch ASC, o.id ASC"
	default:
		return "ORDER BY o.created_at_epoch DESC, o.id DESC"
	}
}

// Search runs an FTS5 MATCH query across title/subtitle/narrative/facts/
// concepts, ranked by bm25 (FTS5's built-in relevance function — lower is
// better, so ORDER BY rank ascending is "best match first"). Ordered by
// rank THEN id, not rank alone: bm25 ties are real (rows with identical
// term-frequency shape score identically), and pagination via LIMIT/OFFSET
// needs a fully deterministic order or two consecutive calls with
// different offsets could return the same row twice or skip one entirely,
// depending only on whatever arbitrary order SQLite happens to visit tied
// rows in.
//
// project scopes the search to one project when non-empty; empty searches
// every project in the store. This store is a single shared database across
// every project ever recorded on the machine (see memory.DefaultDBPath), so an
// unscoped Search is a real cross-project leak — the MCP server (Claude's
// own search_observations tool) always passes the current project; the
// plain `search` CLI subcommand leaves it empty for ad-hoc cross-project
// lookups from a terminal.
func (s *Store) Search(ctx context.Context, project, query, obsType string, limit, offset int, dateStartMs, dateEndMs int64, orderBy string) ([]memory.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	offset = clampNegativeLimit(offset)
	// An empty query means "every observation matching the other
	// filters", not "no results". See searchIsEnumeration.
	enumerate := strings.TrimSpace(query) == ""
	var args []any
	if !enumerate {
		args = append(args, sanitizeFTSQuery(query))
	}
	scope := ""
	if project != "" {
		scope += " AND o.project = ?"
		args = append(args, project)
	}
	// obsType filters on the same small, fixed vocabulary the observer
	// itself ever writes (discovery/change/decision from the real
	// observer prompt, summary from the Stop hook, manual from
	// add_observation) — real claude-mem's own search tool calls this
	// obs_type, one of a handful of filters (date range, offset, sort
	// order) its search used to have that this one didn't; type maps
	// directly onto an existing column with no schema change, and offset
	// (added since) needs no column at all — a plain LIMIT/OFFSET on the
	// existing ORDER BY, previously skipped on a rationale that only ever
	// actually applied to date range and sort order (both added here too,
	// on the same already-indexed created_at_epoch column RecentByProject
	// already queries — no schema change needed for these either, despite
	// this project's own README having claimed otherwise). Comma-separated
	// for multiple, matching real claude-mem's own obs_type docs ("Comma-
	// separated for multiple") and SearchManager.ts's identical split —
	// SessionSearch.ts then branches on array-vs-string to build an IN
	// clause instead of a plain equality one, which this mirrors exactly.
	if types := memory.SplitCommaList(obsType); len(types) == 1 {
		scope += " AND o.type = ?"
		args = append(args, types[0])
	} else if len(types) > 1 {
		placeholders := strings.Repeat("?,", len(types)-1) + "?"
		scope += " AND o.type IN (" + placeholders + ")"
		for _, t := range types {
			args = append(args, t)
		}
	}
	if dateStartMs > 0 {
		scope += " AND o.created_at_epoch >= ?"
		args = append(args, dateStartMs)
	}
	if dateEndMs > 0 {
		scope += " AND o.created_at_epoch <= ?"
		args = append(args, dateEndMs)
	}
	args = append(args, limit, offset)

	// Two shapes, not one query with a neutered predicate: enumeration
	// skips the FTS table entirely rather than asking it to match
	// everything. FTS5 has no "match all" term, and a join against it
	// would restrict results to rows that happen to be indexed.
	from, where := "observations_fts f JOIN observations o ON o.id = f.rowid", "observations_fts MATCH ?"
	if enumerate {
		from, where = "observations o", "1=1"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified, o.next_steps, o.created_at_epoch
		FROM `+from+`
		WHERE `+where+` `+scope+`
		`+searchOrderClause(orderBy, enumerate)+`
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", query, err)
	}
	return scanSearchResults(rows)
}

// RecentByProject returns a project's most recent observations, newest
// first — a plain indexed query (idx_observations_project +
// idx_observations_created), not FTS5; this is "what happened lately here,"
// not a search.
func (s *Store) RecentByProject(ctx context.Context, project string, limit int) ([]memory.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
		FROM observations
		WHERE project = ?
		ORDER BY created_at_epoch DESC, id DESC
		LIMIT ?`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("recent observations for project %q: %w", project, err)
	}
	return scanSearchResults(rows)
}

// BySessionID returns every observation recorded for one session, oldest
// first — the read path for Stop-hook session summarization: the narrative
// arc of what happened, not a ranked search. project scopes it the same
// way Search's does: non-empty restricts to that project, empty means
// every project — session ids are globally unique, but the MCP tool takes
// a caller-supplied id, so the scope is what keeps one project's tool
// from reading another project's session.
func (s *Store) BySessionID(ctx context.Context, project, sessionID string, limit int) ([]memory.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	args := []any{sessionID}
	scope := ""
	if project != "" {
		scope = " AND project = ?"
		args = append(args, project)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
		FROM observations
		WHERE session_id = ?`+scope+`
		ORDER BY created_at_epoch ASC, id ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("observations for session %q: %w", sessionID, err)
	}
	return scanSearchResults(rows)
}

// ObservationsForFile returns observations whose files_read or
// files_modified mentions filePath — the read path for PreToolUse's
// file-context hook: what does memory already know about this specific
// file, not the whole project. json_each is SQLite's JSON1 table-valued
// function for testing array membership; it's compiled into
// modernc.org/sqlite (confirmed by hand, not assumed — see this package's
// doc history).
func (s *Store) ObservationsForFile(ctx context.Context, project, filePath string, limit int) ([]memory.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	// Joins the indexed path table rather than running json_each over
	// every row in the project — see migration 6 for the measurements
	// (46.8ms -> the join, on a hook that BLOCKS every Read).
	//
	// No DISTINCT: observation_files is keyed (observation_id, path), so
	// filtering one path yields at most one row per observation. The old
	// query needed it only because two EXISTS clauses could both hold;
	// keeping it here would re-introduce a sort this shape does not need.
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified, o.next_steps, o.created_at_epoch
		FROM observation_files f
		JOIN observations o ON o.id = f.observation_id
		WHERE f.path = ? AND o.project = ?
		ORDER BY o.created_at_epoch DESC, o.id DESC
		LIMIT ?`, filePath, project, limit)
	if err != nil {
		return nil, fmt.Errorf("observations for file %q: %w", filePath, err)
	}
	return scanSearchResults(rows)
}

// Timeline returns up to depthBefore observations immediately before
// anchorID and up to depthAfter immediately after it, in chronological
// order, with the anchor itself included in the middle — the read path
// for the `timeline` MCP tool, mirroring real claude-mem's own tool of the
// same name ("get context around results"). Ordered by id, not
// created_at_epoch like every other query in this file: id increases
// monotonically with insertion order (SQLite's own rowid), and "the N rows
// immediately before/after this specific row" is an exact relationship
// expressed that way, rather than reconstructed from a timestamp that
// could in principle collide across rows.
//
// Always scoped to the anchor's OWN project, not the caller's project
// argument taken at face value — if a caller passes a project that
// doesn't match the anchor's, that's almost certainly a caller mistake
// (the anchor ID came from a different project's search) and Timeline
// errors rather than silently ignoring it or, worse, ever pulling
// before/after rows from some OTHER project than the one the anchor
// actually lives in.
func (s *Store) Timeline(ctx context.Context, project string, anchorID int64, depthBefore, depthAfter int) ([]memory.SearchResult, error) { // A negative depth is not just "no results" — SQLite's own `LIMIT`
	// treats a negative value as "unlimited," found the hard way against
	// a real database (a naive `LIMIT ?` with depthBefore=-1 returned
	// EVERY row before the anchor, not zero). Clamping the lower bound
	// here, not just the caller's own sanitization (mcpserver.go's
	// runTimeline already defaults <=0 to 10, but that's one caller, not a
	// guarantee), keeps this method itself safe regardless of who calls
	// it or what they pass.
	if depthBefore < 0 {
		depthBefore = 0
	}
	if depthAfter < 0 {
		depthAfter = 0
	}
	if depthBefore > memory.MaxTimelineDepth {
		depthBefore = memory.MaxTimelineDepth
	}
	if depthAfter > memory.MaxTimelineDepth {
		depthAfter = memory.MaxTimelineDepth
	}

	anchorRows, err := s.ByIDs(ctx, []int64{anchorID})
	if err != nil {
		return nil, fmt.Errorf("timeline: %w", err)
	}
	if len(anchorRows) == 0 {
		return nil, fmt.Errorf("timeline: anchor id %d not found", anchorID)
	}
	anchor := anchorRows[0]
	if project != "" && anchor.Project != project {
		return nil, fmt.Errorf("timeline: anchor id %d belongs to a different project", anchorID)
	}

	beforeRows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
		FROM observations
		WHERE id < ? AND project = ?
		ORDER BY id DESC
		LIMIT ?`, anchorID, anchor.Project, depthBefore)
	if err != nil {
		return nil, fmt.Errorf("timeline before id %d: %w", anchorID, err)
	}
	before, err := scanSearchResults(beforeRows)
	if err != nil {
		return nil, fmt.Errorf("timeline before id %d: %w", anchorID, err)
	}
	// beforeRows came back newest-first (closest to the anchor first);
	// reverse in place so the final result reads oldest-to-newest overall.
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}

	afterRows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
		FROM observations
		WHERE id > ? AND project = ?
		ORDER BY id ASC
		LIMIT ?`, anchorID, anchor.Project, depthAfter)
	if err != nil {
		return nil, fmt.Errorf("timeline after id %d: %w", anchorID, err)
	}
	after, err := scanSearchResults(afterRows)
	if err != nil {
		return nil, fmt.Errorf("timeline after id %d: %w", anchorID, err)
	}

	out := make([]memory.SearchResult, 0, len(before)+1+len(after))
	out = append(out, before...)
	out = append(out, anchor)
	out = append(out, after...)
	return out, nil
}

// scanSearchResults drains rows into SearchResults and closes them. It is
// the one scan loop shared by every read path that selects the standard
// column list — Search, RecentByProject, BySessionID, ObservationsForFile,
// ByIDs, Timeline and ObservationsNeedingEmbedding. A query that selects anything else
// (ExportAll adds cost/hash/embedding) keeps its own loop rather than
// bending this one.
//
// Callers MUST select exactly these columns, in this order:
//
//	id, session_id, project, tool_name, type, title, subtitle,
//	facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
func scanSearchResults(rows *sql.Rows) ([]memory.SearchResult, error) {
	defer rows.Close()
	var out []memory.SearchResult
	for rows.Next() {
		var r memory.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified, nextSteps string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified, &nextSteps, &r.CreatedAtEpoch); err != nil {
			return nil, fmt.Errorf("scan observation row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		r.Observation.NextSteps = parseJSONArray(nextSteps)
		out = append(out, r)
	}
	return out, rows.Err()
}

// parseJSONArray inverts jsonArray's encoding. A fact/concept is often a
// full sentence and can contain commas, so this must be real JSON decoding,
// not a naive split(",") — that would silently mis-parse most real facts.
func parseJSONArray(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out) // malformed input yields nil, not a panic
	return out
}

// ByIDs fetches specific observations by ID, in no particular guaranteed
// order beyond what SQLite happens to return — callers that need a stable
// order (e.g. "in the order I asked for them") should sort client-side.
// This is the one lookup shape none of the other read paths cover: every
// other query is "what matches a query/project/session/file," but a caller
// that already has IDs (from a prior search_observations or
// recent_observations call) had no way to re-fetch their full details —
// e.g. facts/concepts/files, which formatSearchResults deliberately omits
// to keep list output short — without re-running the original query and
// hoping the row is still in the page. Unknown IDs are silently omitted
// rather than erroring, the same way a search for a query that matches
// nothing returns an empty slice rather than failing.
func (s *Store) ByIDs(ctx context.Context, ids []int64) ([]memory.SearchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > memory.MaxIDsPerLookup {
		return nil, fmt.Errorf("ByIDs: %d ids exceeds the %d-id limit per call", len(ids), memory.MaxIDsPerLookup)
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
		FROM observations
		WHERE id IN (%s)`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("fetch observations by id: %w", err)
	}
	return scanSearchResults(rows)
}
