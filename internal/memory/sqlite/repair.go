package sqlite

import (
	"context"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// RepairFilePaths implements memory.Backend. The observation_files index is
// only kept in sync by an AFTER INSERT trigger, so its rows for the dropped
// entries are deleted here explicitly.
func (s *Store) RepairFilePaths(ctx context.Context, project string, dryRun bool) (int64, error) {
	where := `(EXISTS (SELECT 1 FROM json_each(files_read) WHERE value NOT LIKE '/%')
	        OR EXISTS (SELECT 1 FROM json_each(files_modified) WHERE value NOT LIKE '/%'))`
	args := []any{}
	if project != "" {
		where += " AND project = ?"
		args = append(args, project)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, files_read, files_modified FROM observations WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("repair file paths: find candidates: %w", err)
	}
	type cand struct {
		id             int64
		read, modified []string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var fr, fm string
		if err := rows.Scan(&c.id, &fr, &fm); err != nil {
			rows.Close()
			return 0, fmt.Errorf("repair file paths: scan: %w", err)
		}
		c.read, _ = memory.SplitRelativePaths(parseJSONArray(fr))
		c.modified, _ = memory.SplitRelativePaths(parseJSONArray(fm))
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("repair file paths: %w", err)
	}
	if dryRun {
		return int64(len(cands)), nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("repair file paths: begin: %w", err)
	}
	defer tx.Rollback()
	for _, c := range cands {
		if _, err := tx.ExecContext(ctx, `UPDATE observations SET files_read = ?, files_modified = ? WHERE id = ?`,
			jsonArray(c.read), jsonArray(c.modified), c.id); err != nil {
			return 0, fmt.Errorf("repair file paths: update observation %d: %w", c.id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM observation_files WHERE observation_id = ? AND path NOT LIKE '/%'`, c.id); err != nil {
			return 0, fmt.Errorf("repair file paths: unindex observation %d: %w", c.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("repair file paths: commit: %w", err)
	}
	return int64(len(cands)), nil
}
