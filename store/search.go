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

// Search runs an FTS5 MATCH query across title/subtitle/narrative/facts/
// concepts, ranked by bm25 (FTS5's built-in relevance function — lower is
// better, so ORDER BY rank ascending is "best match first").
func (s *Store) Search(query string, limit int) ([]SearchResult, error) {
	rows, err := s.db.Query(`
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified
		FROM observations_fts f
		JOIN observations o ON o.id = f.rowid
		WHERE observations_fts MATCH ?
		ORDER BY rank
		LIMIT ?`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fts5 search %q: %w", query, err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&r.Observation.Title, &r.Observation.Subtitle, &facts, &r.Observation.Narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
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
