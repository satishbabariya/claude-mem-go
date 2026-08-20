package postgres

import (
	"database/sql"
	"fmt"

	"claude-mem-go/store"
)

// nullableTextFields scans title/subtitle/narrative — nullable TEXT columns
// in the schema — into sql.NullString rather than a plain Go string. Same
// fix as the SQLite backend's identically-named type, for the identical
// reason: found via a hand-inserted test row with a NULL narrative, which
// the normal Insert() path never produces (it always supplies at least an
// empty string) but the schema permits regardless.
type nullableTextFields struct {
	title, subtitle, narrative sql.NullString
}

func (n nullableTextFields) apply(o *store.Observation) {
	o.Title = n.title.String
	o.Subtitle = n.subtitle.String
	o.Narrative = n.narrative.String
}

// Search runs real Postgres full-text search against the generated
// search_vector column (see schemaSQL), ranked by ts_rank_cd. Unlike the
// SQLite/FTS5 backend, plainto_tsquery tokenizes punctuation (including a
// bare hyphen — the exact case that broke FTS5's grammar) without any
// hand-written sanitizer.
// project scopes the search to one project when non-empty; empty searches
// every project in the store. See store.Store.Search's doc comment for why
// this matters: one shared database can hold observations from every
// project ever recorded on the machine, so an unscoped search is a genuine
// cross-project leak, not just a ranking nuisance.
func (s *Store) Search(project, query string, limit int) ([]store.SearchResult, error) {
	scope := ""
	args := []any{query}
	if project != "" {
		scope = "AND project = $3"
		args = append(args, limit, project)
	} else {
		args = append(args, limit)
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE search_vector @@ plainto_tsquery('english', $1) `+scope+`
		ORDER BY ts_rank_cd(search_vector, plainto_tsquery('english', $1)) DESC
		LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("full text search %q: %w", query, err)
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentByProject returns a project's most recent observations, newest
// first — the plain-index read path for SessionStart context injection,
// not a search.
func (s *Store) RecentByProject(project string, limit int) ([]store.SearchResult, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE project = $1
		ORDER BY created_at_epoch DESC, id DESC
		LIMIT $2`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("recent observations for project %q: %w", project, err)
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan recent observation: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// BySessionID returns every observation recorded for one session, oldest
// first — the read path for Stop-hook session summarization.
func (s *Store) BySessionID(sessionID string, limit int) ([]store.SearchResult, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE session_id = $1
		ORDER BY created_at_epoch ASC, id ASC
		LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("observations for session %q: %w", sessionID, err)
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan session observation: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ByIDs fetches specific observations by ID — the one lookup shape none of
// the other read paths cover, for a caller that already has IDs (from a
// prior Search/RecentByProject call) and wants full details (facts,
// narrative, concepts, files) that the abbreviated list formats omit.
// Unknown IDs are silently omitted rather than erroring. `= ANY($1)` with a
// native Go []int64 arg is pgx's own array support (see pgx/v5's stdlib
// driver docs) — no hand-built placeholder list needed, unlike SQLite's
// `IN (?,?,...)` (database/sql gives SQLite no equivalent to bind a whole
// slice as one placeholder).
func (s *Store) ByIDs(ids []int64) ([]store.SearchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch observations by id: %w", err)
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan observation by id: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ObservationsForFile returns observations whose files_read or
// files_modified mentions filePath — the read path for PreToolUse's
// file-context hook. `?` is JSONB's native "does this string exist as a
// top-level array element" operator — the Postgres analog of SQLite's
// json_each membership check, and does not conflict with pgx's $N
// placeholder syntax (pgx never treats a bare `?` as a placeholder).
func (s *Store) ObservationsForFile(project, filePath string, limit int) ([]store.SearchResult, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE project = $1
		  AND (files_read ? $2 OR files_modified ? $2)
		ORDER BY created_at_epoch DESC, id DESC
		LIMIT $3`, project, filePath, limit)
	if err != nil {
		return nil, fmt.Errorf("observations for file %q: %w", filePath, err)
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan file-context observation: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}
