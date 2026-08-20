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
CREATE TRIGGER IF NOT EXISTS observations_ad AFTER DELETE ON observations BEGIN
	INSERT INTO observations_fts(observations_fts, rowid, title, subtitle, narrative, facts, concepts)
	VALUES ('delete', old.id, old.title, old.subtitle, old.narrative, old.facts, old.concepts);
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

// Search runs an FTS5 MATCH query across title/subtitle/narrative/facts/
// concepts, ranked by bm25 (FTS5's built-in relevance function — lower is
// better, so ORDER BY rank ascending is "best match first").
//
// project scopes the search to one project when non-empty; empty searches
// every project in the store. This store is a single shared database across
// every project ever recorded on the machine (see DefaultDBPath), so an
// unscoped Search is a real cross-project leak — the MCP server (Claude's
// own search_observations tool) always passes the current project; the
// plain `search` CLI subcommand leaves it empty for ad-hoc cross-project
// lookups from a terminal.
func (s *Store) Search(project, query string, limit int) ([]SearchResult, error) {
	args := []any{sanitizeFTSQuery(query)}
	scope := ""
	if project != "" {
		scope = "AND o.project = ?"
		args = append(args, project)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified
		FROM observations_fts f
		JOIN observations o ON o.id = f.rowid
		WHERE observations_fts MATCH ? `+scope+`
		ORDER BY rank
		LIMIT ?`, args...)
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

// parseJSONArray inverts jsonArray's encoding. A fact/concept is often a
// full sentence and can contain commas, so this must be real JSON decoding,
// not a naive split(",") — that would silently mis-parse most real facts.
func parseJSONArray(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out) // malformed input yields nil, not a panic
	return out
}
