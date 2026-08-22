package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// ExportAll returns up to limit observations with id > afterID, ordered by
// id ascending. Callers loop — passing the previous page's last ID as the
// next afterID — until a page comes back with fewer than limit rows.
// Paginated by design, not "return everything at once": exporting a store
// with a very large history must not require loading it all into memory
// first. There was no way to get data out of this store at all before
// this — no backup story, no way to move data between the SQLite and
// Postgres backends.
func (s *Store) ExportAll(ctx context.Context, afterID int64, limit int) ([]memory.ExportRow, error) {
	limit = clampNegativeLimit(limit)
	// LEFT JOIN, not INNER: most observations have no row in
	// observation_vectors at all (never embedded), and that must not
	// exclude them from the export.
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified, o.next_steps,
		       o.cost_usd, o.created_at, o.created_at_epoch, o.content_hash,
		       v.dims, v.embedding
		FROM observations o
		LEFT JOIN observation_vectors v ON v.observation_id = o.id
		WHERE o.id > ?
		ORDER BY o.id ASC
		LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("export observations after id %d: %w", afterID, err)
	}
	defer rows.Close()

	var out []memory.ExportRow
	for rows.Next() {
		var r memory.ExportRow
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified, nextSteps string
		var dims sql.NullInt64
		var embeddingBlob []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative, &concepts, &filesRead, &filesModified, &nextSteps,
			&r.CostUSD, &r.CreatedAt, &r.CreatedAtEpoch, &r.ContentHash,
			&dims, &embeddingBlob); err != nil {
			return nil, fmt.Errorf("scan export row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
		r.Observation.NextSteps = parseJSONArray(nextSteps)
		if dims.Valid {
			vec, err := decodeVector(embeddingBlob, int(dims.Int64))
			if err != nil {
				return nil, fmt.Errorf("decode embedding for observation %d: %w", r.ID, err)
			}
			r.Embedding = vec
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ImportRow re-inserts a previously exported row, preserving its original
// memory.ContentHash (the same idempotent-dedup guarantee Insert provides — a
// row already imported is a no-op, not a duplicate), its original
// CreatedAt/CreatedAtEpoch, and its embedding, if it had one — a restore
// should be a full restore, not one that quietly leaves every observation
// unsearchable by meaning.
func (s *Store) ImportRow(ctx context.Context, row memory.ExportRow) (memory.InsertResult, error) { // Validated but deliberately not used: this column is TEXT, so the
	// original string is what gets stored (see memory.ParseExportCreatedAt for
	// why that matters for byte-identical round trips). The call is here
	// purely so a malformed timestamp is rejected on this backend exactly
	// as it already was on Postgres.
	if _, err := memory.ParseExportCreatedAt(row.CreatedAt); err != nil {
		return memory.InsertResult{}, err
	}
	res, err := s.insertRow(ctx, row.SessionID, row.Project, row.ToolName, row.ContentHash,
		row.Observation, row.CostUSD, row.CreatedAt, row.CreatedAtEpoch)
	if err != nil {
		return res, err
	}
	if res.Inserted && len(row.Embedding) > 0 {
		if err := s.SaveEmbedding(ctx, res.ID, row.Embedding); err != nil {
			return res, fmt.Errorf("import observation %d: save embedding: %w", res.ID, err)
		}
	}
	return res, nil
}
