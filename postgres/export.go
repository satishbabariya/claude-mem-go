package postgres

import (
	"fmt"
	"time"

	"claude-mem-go/store"
)

// ExportAll is the Postgres half of store.Backend's export/backup
// surface — see store.Store.ExportAll's doc comment for the pagination
// rationale, shared by both backends.
func (s *Store) ExportAll(afterID int64, limit int) ([]store.ExportRow, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified,
		       cost_usd, created_at, created_at_epoch, content_hash
		FROM observations
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("export observations after id %d: %w", afterID, err)
	}
	defer rows.Close()

	var out []store.ExportRow
	for rows.Next() {
		var r store.ExportRow
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified []byte
		var createdAt time.Time
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative, &concepts, &filesRead, &filesModified,
			&r.CostUSD, &createdAt, &r.CreatedAtEpoch, &r.ContentHash); err != nil {
			return nil, fmt.Errorf("scan export row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		// CreatedAt travels as an RFC3339 string in ExportRow so both
		// backends' export files share one on-the-wire format regardless
		// of whether the source column is SQLite TEXT or Postgres
		// TIMESTAMPTZ — this is what lets export/import double as the
		// SQLite<->Postgres migration path, not just same-backend backup.
		r.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ImportRow re-inserts a previously exported row — see store.Store's
// ImportRow doc comment for the idempotency/timestamp-preservation
// rationale, identical here.
func (s *Store) ImportRow(row store.ExportRow) (store.InsertResult, error) {
	createdAt, err := time.Parse(time.RFC3339, row.CreatedAt)
	if err != nil {
		return store.InsertResult{}, fmt.Errorf("parse CreatedAt %q: %w", row.CreatedAt, err)
	}
	return s.insertRow(row.SessionID, row.Project, row.ToolName, row.ContentHash,
		row.Observation, row.CostUSD, createdAt, row.CreatedAtEpoch)
}
