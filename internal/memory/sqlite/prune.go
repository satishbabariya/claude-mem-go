package sqlite

import "fmt"

// Prune deletes observations older than cutoffEpoch (created_at_epoch <
// cutoffEpoch — a Unix seconds timestamp), scoped to one project when
// non-empty, every project when empty. dryRun counts what WOULD be
// deleted without deleting anything, so a caller can preview impact
// before running for real — this is the only genuinely destructive
// operation this package exposes, and there was no retention story at all
// before this: the store grows forever otherwise.
//
// FTS5's shadow table stays consistent automatically: the observations_ad
// trigger (search.go) fires per deleted row regardless of whether the
// DELETE is single-row or bulk. observation_vectors cleans up the same
// way via its ON DELETE CASCADE foreign key (vector.go).
func (s *Store) Prune(project string, cutoffEpoch int64, dryRun bool) (int64, error) {
	args := []any{cutoffEpoch}
	scope := ""
	if project != "" {
		scope = "AND project = ?"
		args = append(args, project)
	}

	if dryRun {
		var n int64
		err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE created_at_epoch < ? `+scope, args...).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count observations older than cutoff: %w", err)
		}
		return n, nil
	}

	res, err := s.db.Exec(`DELETE FROM observations WHERE created_at_epoch < ? `+scope, args...)
	if err != nil {
		return 0, fmt.Errorf("prune observations older than cutoff: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected after prune: %w", err)
	}
	return n, nil
}
