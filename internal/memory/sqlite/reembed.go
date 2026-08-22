package sqlite

import (
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// ObservationsNeedingEmbedding returns up to limit observations (with
// id > afterID, oldest first) that either have no embedding at all or
// whose stored embedding dimension doesn't match expectedDims — the read
// path for the `reembed` CLI command, which is the remediation half of
// HealthDetails' embedding_dims_consistent=false finding: detecting a
// stale/inconsistent embedding is one thing, but there was no way to
// actually FIX it (re-embed the affected rows with the current model)
// short of re-ingesting from scratch. Also naturally catches observations
// that were simply never embedded at all (no embed model configured at
// capture time), which is the more common real-world case.
//
// project scopes it to one project when non-empty, same convention as
// every other read path. Paginated like ExportAll, for the same reason:
// a store with a large history must not require loading every candidate
// row into memory at once.
func (s *Store) ObservationsNeedingEmbedding(project string, expectedDims int64, afterID int64, limit int) ([]memory.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	args := []any{afterID, expectedDims}
	scope := ""
	if project != "" {
		scope = "AND o.project = ?"
		args = append(args, project)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified, o.created_at_epoch
		FROM observations o
		LEFT JOIN observation_vectors v ON v.observation_id = o.id
		WHERE o.id > ?
		  AND (v.observation_id IS NULL OR v.dims != ?)
		  `+scope+`
		ORDER BY o.id ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("observations needing embedding after id %d: %w", afterID, err)
	}
	defer rows.Close()

	var out []memory.SearchResult
	for rows.Next() {
		var r memory.SearchResult
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative,
			&concepts, &filesRead, &filesModified, &r.CreatedAtEpoch); err != nil {
			return nil, fmt.Errorf("scan observation needing embedding: %w", err)
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
