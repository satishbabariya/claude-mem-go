package postgres

import "fmt"

// Prune is the Postgres half of store.Backend's retention story — see
// store.Store.Prune's doc comment for the full rationale. No separate
// shadow table or cascaded rows to worry about here: search_vector is a
// generated column on the same row, and embedding is a plain column on the
// same row, so a plain DELETE cleans up everything in one statement.
func (s *Store) Prune(project string, cutoffEpoch int64, dryRun bool) (int64, error) {
	args := []any{cutoffEpoch}
	scope := ""
	if project != "" {
		scope = "AND project = $2"
		args = append(args, project)
	}

	if dryRun {
		var n int64
		err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE created_at_epoch < $1 `+scope, args...).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count observations older than cutoff: %w", err)
		}
		return n, nil
	}

	res, err := s.db.Exec(`DELETE FROM observations WHERE created_at_epoch < $1 `+scope, args...)
	if err != nil {
		return 0, fmt.Errorf("prune observations older than cutoff: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected after prune: %w", err)
	}
	return n, nil
}
