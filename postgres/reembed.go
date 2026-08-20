package postgres

import (
	"fmt"

	"claude-mem-go/store"
)

// ObservationsNeedingEmbedding mirrors store.Store's method of the same
// name — the remediation read path for the reembed CLI command. Unlike
// the SQLite backend, a genuine dimension mismatch can't actually occur
// here (the embedding column is a fixed vector(N) type — see
// HealthDetails' doc comment), but a NULL embedding (never embedded at
// all) is exactly as real a case here as there, so this still needs to
// exist and behave identically for callers.
func (s *Store) ObservationsNeedingEmbedding(project string, expectedDims int64, afterID int64, limit int) ([]store.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	scope := ""
	args := []any{afterID, expectedDims, limit}
	if project != "" {
		scope = "AND project = $4"
		args = append(args, project)
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified
		FROM observations
		WHERE id > $1
		  AND (embedding IS NULL OR vector_dims(embedding) != $2)
		  `+scope+`
		ORDER BY id ASC
		LIMIT $3`, args...)
	if err != nil {
		return nil, fmt.Errorf("observations needing embedding after id %d: %w", afterID, err)
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
			return nil, fmt.Errorf("scan observation needing embedding: %w", err)
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
