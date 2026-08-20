package store

import "fmt"

// ExportRow is one observation as read back by ExportAll — everything
// needed to reinsert it via ImportRow, preserving its original identity
// (ContentHash, the idempotency key) and timing (CreatedAt/CreatedAtEpoch),
// rather than re-stamping "now" the way a normal capture does.
type ExportRow struct {
	ID             int64       `json:"id"`
	SessionID      string      `json:"session_id"`
	Project        string      `json:"project"`
	ToolName       string      `json:"tool_name"`
	ContentHash    string      `json:"content_hash"`
	Observation    Observation `json:"observation"`
	CostUSD        float64     `json:"cost_usd"`
	CreatedAt      string      `json:"created_at"`
	CreatedAtEpoch int64       `json:"created_at_epoch"`
}

// ExportAll returns up to limit observations with id > afterID, ordered by
// id ascending. Callers loop — passing the previous page's last ID as the
// next afterID — until a page comes back with fewer than limit rows.
// Paginated by design, not "return everything at once": exporting a store
// with a very large history must not require loading it all into memory
// first. There was no way to get data out of this store at all before
// this — no backup story, no way to move data between the SQLite and
// Postgres backends.
func (s *Store) ExportAll(afterID int64, limit int) ([]ExportRow, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified,
		       cost_usd, created_at, created_at_epoch, content_hash
		FROM observations
		WHERE id > ?
		ORDER BY id ASC
		LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("export observations after id %d: %w", afterID, err)
	}
	defer rows.Close()

	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative, &concepts, &filesRead, &filesModified,
			&r.CostUSD, &r.CreatedAt, &r.CreatedAtEpoch, &r.ContentHash); err != nil {
			return nil, fmt.Errorf("scan export row: %w", err)
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

// ImportRow re-inserts a previously exported row, preserving its original
// ContentHash (the same idempotent-dedup guarantee Insert provides — a
// row already imported is a no-op, not a duplicate) and its original
// CreatedAt/CreatedAtEpoch, so a restore reflects when things actually
// happened rather than when they were re-imported.
func (s *Store) ImportRow(row ExportRow) (InsertResult, error) {
	return s.insertRow(row.SessionID, row.Project, row.ToolName, row.ContentHash,
		row.Observation, row.CostUSD, row.CreatedAt, row.CreatedAtEpoch)
}
