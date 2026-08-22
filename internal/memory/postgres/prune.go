package postgres

import (
	"context"
	"fmt"
)

// Prune is the Postgres half of memory.Backend's retention story — see
// sqlite.Store.Prune's doc comment for the full rationale. No separate
// shadow table or cascaded rows to worry about here: search_vector is a
// generated column on the same row, and embedding is a plain column on the
// same row, so a plain DELETE cleans up everything in one statement.
// Stored user prompts older than the same cutoff are deleted too, not
// counted — the returned figure has always meant observations.
func (s *Store) Prune(ctx context.Context, project string, cutoffEpoch int64, dryRun bool) (int64, error) {
	args := []any{cutoffEpoch}
	scope := ""
	if project != "" {
		scope = "AND project = $2"
		args = append(args, project)
	}

	if dryRun {
		var n int64
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM observations WHERE created_at_epoch < $1 `+scope, args...).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count observations older than cutoff: %w", err)
		}
		return n, nil
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM observations WHERE created_at_epoch < $1 `+scope, args...)
	if err != nil {
		return 0, fmt.Errorf("prune observations older than cutoff: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected after prune: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_prompts WHERE created_at_epoch < $1 `+scope, args...); err != nil {
		return n, fmt.Errorf("prune user prompts older than cutoff: %w", err)
	}
	return n, nil
}
