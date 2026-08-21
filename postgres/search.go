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

// clampNegativeLimit guards every LIMIT-bounded query in this file — see
// store.clampNegativeLimit's doc comment for the full story (found
// against the SQLite backend first). This backend fails differently for
// the identical root cause: Postgres rejects a negative LIMIT outright
// with a real "LIMIT must not be negative" error rather than SQLite's
// silent "unlimited," confirmed directly against the live container —
// still worth clamping here rather than letting that raw driver error
// reach whatever called this.
func clampNegativeLimit(limit int) int {
	if limit < 0 {
		return 0
	}
	return limit
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
// cross-project leak, not just a ranking nuisance. obsType additionally
// filters to one observation type when non-empty — see the SQLite
// backend's identically-named parameter for the real, small vocabulary
// this filters against.
//
// Builds its placeholder numbers dynamically (len(args) as each optional
// filter is appended) rather than hardcoding $3/$4/etc.: two independent
// optional filters (project, obsType) means a fixed numbering scheme
// would need to track which combination of filters is present to know
// what number LIMIT actually lands on — a real source of off-by-one
// mistakes for exactly two conditionals, let alone more later.
//
// Ordered by rank THEN id, not rank alone: ts_rank_cd ties are real, and
// pagination via LIMIT/OFFSET needs a fully deterministic order or two
// consecutive calls at different offsets could return the same row twice
// or skip one entirely, depending on whatever arbitrary order Postgres
// happens to visit tied rows in — same reasoning as the SQLite backend's
// identical tiebreaker, independently necessary here.
func (s *Store) Search(project, query, obsType string, limit, offset int) ([]store.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	offset = clampNegativeLimit(offset)
	args := []any{query}
	scope := ""
	if project != "" {
		args = append(args, project)
		scope += fmt.Sprintf(" AND project = $%d", len(args))
	}
	if obsType != "" {
		args = append(args, obsType)
		scope += fmt.Sprintf(" AND type = $%d", len(args))
	}
	args = append(args, limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))
	args = append(args, offset)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args))
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE search_vector @@ plainto_tsquery('english', $1) `+scope+`
		ORDER BY ts_rank_cd(search_vector, plainto_tsquery('english', $1)) DESC, id
		LIMIT `+limitPlaceholder+`
		OFFSET `+offsetPlaceholder, args...)
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
	limit = clampNegativeLimit(limit)
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
	limit = clampNegativeLimit(limit)
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
// slice as one placeholder), so this backend doesn't actually hit the same
// "too many SQL variables" failure SQLite's ByIDs does — but the same
// store.MaxIDsPerLookup bound applies anyway, for parity: a caller
// shouldn't see a different effective limit depending on which backend
// happens to be active, and no legitimate caller needs more than a page of
// IDs from a single detail lookup regardless of backend.
func (s *Store) ByIDs(ids []int64) ([]store.SearchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > store.MaxIDsPerLookup {
		return nil, fmt.Errorf("ByIDs: %d ids exceeds the %d-id limit per call", len(ids), store.MaxIDsPerLookup)
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
	limit = clampNegativeLimit(limit)
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

// Timeline mirrors store.Store's Timeline exactly, including the same
// "always scoped to the anchor's own project" safety rule and the same
// store.MaxTimelineDepth cap — see its doc comment for the full
// reasoning. Ordered by id, same as the SQLite backend: BIGSERIAL
// increases monotonically with insertion order here too, so "before/after
// this row" is exact without needing a timestamp comparison.
func (s *Store) Timeline(project string, anchorID int64, depthBefore, depthAfter int) ([]store.SearchResult, error) {
	// Postgres's own LIMIT rejects a negative value outright ("LIMIT must
	// not be negative," confirmed against the real container) rather than
	// SQLite's "unlimited" — a different failure mode from the same root
	// cause (see the SQLite backend's identical clamp for the full
	// story), but still worth guarding here directly rather than relying
	// on the query simply erroring: a clamp is a better outcome than a
	// raw driver error reaching whatever called this.
	if depthBefore < 0 {
		depthBefore = 0
	}
	if depthAfter < 0 {
		depthAfter = 0
	}
	if depthBefore > store.MaxTimelineDepth {
		depthBefore = store.MaxTimelineDepth
	}
	if depthAfter > store.MaxTimelineDepth {
		depthAfter = store.MaxTimelineDepth
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
		WHERE id < $1 AND project = $2
		ORDER BY id DESC
		LIMIT $3`, anchorID, anchor.Project, depthBefore)
	if err != nil {
		return nil, fmt.Errorf("timeline before id %d: %w", anchorID, err)
	}
	before, err := scanTimelineRows(beforeRows)
	if err != nil {
		return nil, fmt.Errorf("timeline before id %d: %w", anchorID, err)
	}
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}

	afterRows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE id > $1 AND project = $2
		ORDER BY id ASC
		LIMIT $3`, anchorID, anchor.Project, depthAfter)
	if err != nil {
		return nil, fmt.Errorf("timeline after id %d: %w", anchorID, err)
	}
	after, err := scanTimelineRows(afterRows)
	if err != nil {
		return nil, fmt.Errorf("timeline after id %d: %w", anchorID, err)
	}

	out := make([]store.SearchResult, 0, len(before)+1+len(after))
	out = append(out, before...)
	out = append(out, anchor)
	out = append(out, after...)
	return out, nil
}

// scanTimelineRows is Timeline's own scan helper — see the SQLite
// backend's identically-named function for why it isn't shared more
// broadly across this file's other queries.
func scanTimelineRows(rows *sql.Rows) ([]store.SearchResult, error) {
	defer rows.Close()
	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan timeline row: %w", err)
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
