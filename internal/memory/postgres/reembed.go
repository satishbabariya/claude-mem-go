package postgres

import (
	"context"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// ObservationsNeedingEmbedding mirrors sqlite.Store's method of the same
// name — the remediation read path for the reembed CLI command.
//
// An earlier version of this comment claimed a genuine dimension
// mismatch "can't actually occur here" because the column is a fixed
// vector(N). That was wrong, and measured wrong: calling this with an
// expectedDims that differs from the column's width returns EVERY
// embedded row as needing re-embedding, and SaveEmbedding then rejects
// each one — so reembed, the designated remediation path, is a dead end
// on this backend whenever the configured model's output size doesn't
// match the store. The column being fixed-width is exactly what causes
// that, not what prevents it. SaveEmbedding now says so explicitly
// instead of surfacing pgvector's bare dimension error.
func (s *Store) ObservationsNeedingEmbedding(ctx context.Context, project string, expectedDims int64, afterID int64, limit int) ([]memory.SearchResult, error) {
	limit = clampNegativeLimit(limit)
	scope := ""
	args := []any{afterID, expectedDims, limit}
	if project != "" {
		scope = "AND project = $4"
		args = append(args, project)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps, created_at_epoch
		FROM observations
		WHERE id > $1
		  AND (embedding IS NULL OR vector_dims(embedding) != $2)
		  `+scope+`
		ORDER BY id ASC
		LIMIT $3`, args...)
	if err != nil {
		return nil, fmt.Errorf("observations needing embedding after id %d: %w", afterID, err)
	}
	return scanSearchResults(rows)
}
