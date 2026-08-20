package postgres

import (
	"fmt"

	"claude-mem-go/store"
)

// Search runs real Postgres full-text search against the generated
// search_vector column (see schemaSQL), ranked by ts_rank_cd. Unlike the
// SQLite/FTS5 backend, plainto_tsquery tokenizes punctuation (including a
// bare hyphen — the exact case that broke FTS5's grammar) without any
// hand-written sanitizer.
func (s *Store) Search(query string, limit int) ([]store.SearchResult, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE search_vector @@ plainto_tsquery('english', $1)
		ORDER BY ts_rank_cd(search_vector, plainto_tsquery('english', $1)) DESC
		LIMIT $2`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("full text search %q: %w", query, err)
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var r store.SearchResult
		var facts, concepts, filesRead, filesModified []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&r.Observation.Title, &r.Observation.Subtitle, &facts, &r.Observation.Narrative,
			&concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		out = append(out, r)
	}
	return out, rows.Err()
}
