package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// Stats implements memory.Backend.Stats for Postgres. Same shape as the
// SQLite implementation, with the embedding count coming from the
// embedding column rather than a side table.
func (s *Store) Stats(ctx context.Context) (memory.StoreStats, error) {
	var out memory.StoreStats
	out.ByType = map[string]int{}

	var oldest, newest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*), count(DISTINCT project), count(DISTINCT session_id),
		       min(created_at_epoch), max(created_at_epoch),
		       count(*) FILTER (WHERE embedding IS NOT NULL)
		FROM observations`).Scan(&out.Observations, &out.Projects, &out.Sessions,
		&oldest, &newest, &out.Embedded)
	if err != nil {
		return memory.StoreStats{}, fmt.Errorf("stats: aggregate counts: %w", err)
	}
	if oldest.Valid {
		out.OldestEpochMs = oldest.Int64
	}
	if newest.Valid {
		out.NewestEpochMs = newest.Int64
	}

	rows, err := s.db.QueryContext(ctx, `SELECT type, count(*) FROM observations GROUP BY type`)
	if err != nil {
		return memory.StoreStats{}, fmt.Errorf("stats: count by type: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return memory.StoreStats{}, fmt.Errorf("stats: scan count by type: %w", err)
		}
		out.ByType[t] = n
	}
	if err := rows.Err(); err != nil {
		return memory.StoreStats{}, fmt.Errorf("stats: count by type: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM user_prompts`).Scan(&out.Prompts); err != nil {
		return memory.StoreStats{}, fmt.Errorf("stats: count prompts: %w", err)
	}
	return out, nil
}
