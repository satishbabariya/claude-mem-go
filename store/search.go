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
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// SearchResult is one FTS5 match, joined back to its full observation.
type SearchResult struct {
	ID          int64
	Observation Observation
	SessionID   string
	Project     string
	ToolName    string
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

func (n nullableTextFields) apply(o *Observation) {
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
// defense-in-depth for the Backend contract itself, the same reasoning as
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
func searchOrderClause(orderBy string) string {
	switch orderBy {
	case "", "relevance":
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
// every project ever recorded on the machine (see DefaultDBPath), so an
// unscoped Search is a real cross-project leak — the MCP server (Claude's
// own search_observations tool) always passes the current project; the
// plain `search` CLI subcommand leaves it empty for ad-hoc cross-project
// lookups from a terminal.
func (s *Store) Search(project, query, obsType string, limit, offset int, dateStartMs, dateEndMs int64, orderBy string) ([]SearchResult, error) {
	limit = clampNegativeLimit(limit)
	offset = clampNegativeLimit(offset)
	args := []any{sanitizeFTSQuery(query)}
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
	// this project's own README having claimed otherwise).
	if obsType != "" {
		scope += " AND o.type = ?"
		args = append(args, obsType)
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
	rows, err := s.db.Query(`
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified
		FROM observations_fts f
		JOIN observations o ON o.id = f.rowid
		WHERE observations_fts MATCH ? `+scope+`
		`+searchOrderClause(orderBy)+`
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("fts5 search %q: %w", query, err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentByProject returns a project's most recent observations, newest
// first — a plain indexed query (idx_observations_project +
// idx_observations_created), not FTS5; this is "what happened lately here,"
// not a search.
func (s *Store) RecentByProject(project string, limit int) ([]SearchResult, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE project = ?
		ORDER BY created_at_epoch DESC, id DESC
		LIMIT ?`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("recent observations for project %q: %w", project, err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan recent observation: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// BySessionID returns every observation recorded for one session, oldest
// first — the read path for Stop-hook session summarization: the narrative
// arc of what happened, not a ranked search.
func (s *Store) BySessionID(sessionID string, limit int) ([]SearchResult, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE session_id = ?
		ORDER BY created_at_epoch ASC, id ASC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("observations for session %q: %w", sessionID, err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan session observation: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ObservationsForFile returns observations whose files_read or
// files_modified mentions filePath — the read path for PreToolUse's
// file-context hook: what does memory already know about this specific
// file, not the whole project. json_each is SQLite's JSON1 table-valued
// function for testing array membership; it's compiled into
// modernc.org/sqlite (confirmed by hand, not assumed — see this package's
// doc history).
func (s *Store) ObservationsForFile(project, filePath string, limit int) ([]SearchResult, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.Query(`
		SELECT DISTINCT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified
		FROM observations o
		WHERE o.project = ?
		  AND (
		    EXISTS (SELECT 1 FROM json_each(o.files_read) WHERE json_each.value = ?)
		    OR EXISTS (SELECT 1 FROM json_each(o.files_modified) WHERE json_each.value = ?)
		  )
		ORDER BY o.created_at_epoch DESC, o.id DESC
		LIMIT ?`, project, filePath, filePath, limit)
	if err != nil {
		return nil, fmt.Errorf("observations for file %q: %w", filePath, err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan file-context observation: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MaxTimelineDepth bounds Timeline's depthBefore/depthAfter — the same
// "no legitimate caller needs more than a page" reasoning as
// MaxIDsPerLookup, and for a query built from a caller-controlled integer
// rather than a caller-controlled slice length, so there's no driver-level
// placeholder-count failure mode here to reproduce; this cap exists purely
// to keep one MCP call bounded, not to work around a driver limit.
const MaxTimelineDepth = 100

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
func (s *Store) Timeline(project string, anchorID int64, depthBefore, depthAfter int) ([]SearchResult, error) {
	// A negative depth is not just "no results" — SQLite's own `LIMIT`
	// treats a negative value as "unlimited," found the hard way against
	// a real database (a naive `LIMIT ?` with depthBefore=-1 returned
	// EVERY row before the anchor, not zero). Clamping the lower bound
	// here, not just the caller's own sanitization (mcpserver.go's
	// runTimeline already defaults <=0 to 3, but that's one caller, not a
	// guarantee), keeps this method itself safe regardless of who calls
	// it or what they pass.
	if depthBefore < 0 {
		depthBefore = 0
	}
	if depthAfter < 0 {
		depthAfter = 0
	}
	if depthBefore > MaxTimelineDepth {
		depthBefore = MaxTimelineDepth
	}
	if depthAfter > MaxTimelineDepth {
		depthAfter = MaxTimelineDepth
	}

	anchorRows, err := s.ByIDs([]int64{anchorID})
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

	beforeRows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE id < ? AND project = ?
		ORDER BY id DESC
		LIMIT ?`, anchorID, anchor.Project, depthBefore)
	if err != nil {
		return nil, fmt.Errorf("timeline before id %d: %w", anchorID, err)
	}
	before, err := scanTimelineRows(beforeRows)
	if err != nil {
		return nil, fmt.Errorf("timeline before id %d: %w", anchorID, err)
	}
	// beforeRows came back newest-first (closest to the anchor first);
	// reverse in place so the final result reads oldest-to-newest overall.
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}

	afterRows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE id > ? AND project = ?
		ORDER BY id ASC
		LIMIT ?`, anchorID, anchor.Project, depthAfter)
	if err != nil {
		return nil, fmt.Errorf("timeline after id %d: %w", anchorID, err)
	}
	after, err := scanTimelineRows(afterRows)
	if err != nil {
		return nil, fmt.Errorf("timeline after id %d: %w", anchorID, err)
	}

	out := make([]SearchResult, 0, len(before)+1+len(after))
	out = append(out, before...)
	out = append(out, anchor)
	out = append(out, after...)
	return out, nil
}

// scanTimelineRows is Timeline's own scan helper, not shared with the rest
// of this file's queries (each already has its own inline scan loop,
// matching this file's existing convention) — factored out only because
// Timeline's before/after queries are otherwise identical to each other
// and it would otherwise be the third copy of the exact same block within
// one function.
func scanTimelineRows(rows *sql.Rows) ([]SearchResult, error) {
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan timeline row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
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

// MaxIDsPerLookup bounds a single ByIDs call, enforced by both backends —
// found the hard way, not anticipated: a real test against this project's
// own SQLite driver (modernc.org/sqlite) showed 100,000 IDs failing
// outright with "SQL logic error: too many SQL variables" once the
// hand-built IN (?,?,...) placeholder list crossed the driver's real
// limit, rather than degrading gracefully. Set well below where that
// limit actually starts (confirmed empirically between 10,000 and
// 100,000) and matching the "max 100" convention every other MCP tool's
// own limit argument already uses (see mcpserver.go), since no legitimate
// caller needs more than a page of IDs at once — this is a detail lookup
// for results a search already returned, not a bulk export (see export/
// import for that).
const MaxIDsPerLookup = 100

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
func (s *Store) ByIDs(ids []int64) ([]SearchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxIDsPerLookup {
		return nil, fmt.Errorf("ByIDs: %d ids exceeds the %d-id limit per call", len(ids), MaxIDsPerLookup)
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE id IN (%s)`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("fetch observations by id: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan observation by id: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}
