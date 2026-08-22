package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// RepairFilePaths implements memory.Backend. files_read/files_modified are
// JSONB here and indexed by GIN directly, so rewriting the arrays is the
// whole repair — there is no side table to keep in step.
func (s *Store) RepairFilePaths(ctx context.Context, project string, dryRun bool) (int64, error) {
	where := `(EXISTS (SELECT 1 FROM jsonb_array_elements_text(files_read) v WHERE v NOT LIKE '/%')
	        OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(files_modified) v WHERE v NOT LIKE '/%'))`
	args := []any{}
	if project != "" {
		where += " AND project = $1"
		args = append(args, project)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, files_read, files_modified FROM observations WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("repair file paths: find candidates: %w", err)
	}
	type cand struct {
		id             int64
		read, modified []byte
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var fr, fm []byte
		if err := rows.Scan(&c.id, &fr, &fm); err != nil {
			rows.Close()
			return 0, fmt.Errorf("repair file paths: scan: %w", err)
		}
		keepR, _ := memory.SplitRelativePaths(jsonDecode(fr))
		keepM, _ := memory.SplitRelativePaths(jsonDecode(fm))
		c.read, _ = json.Marshal(keepR)
		c.modified, _ = json.Marshal(keepM)
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
		if _, err := tx.ExecContext(ctx, `UPDATE observations SET files_read = $1, files_modified = $2 WHERE id = $3`,
			c.read, c.modified, c.id); err != nil {
			return 0, fmt.Errorf("repair file paths: update observation %d: %w", c.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("repair file paths: commit: %w", err)
	}
	return int64(len(cands)), nil
}
