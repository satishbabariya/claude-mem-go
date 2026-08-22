package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// ExportAll is the Postgres half of memory.Backend's export/backup
// surface — see sqlite.Store.ExportAll's doc comment for the pagination
// rationale, shared by both backends, and its doc comment on ExportRow
// for why the embedding needs to travel too, not just the observation's
// content.
func (s *Store) ExportAll(ctx context.Context, afterID int64, limit int) ([]memory.ExportRow, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, tool_name, type, title, subtitle,
		       facts, narrative, concepts, files_read, files_modified, next_steps,
		       cost_usd, created_at, created_at_epoch, content_hash, embedding
		FROM observations
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("export observations after id %d: %w", afterID, err)
	}
	defer rows.Close()

	var out []memory.ExportRow
	for rows.Next() {
		var r memory.ExportRow
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified, nextSteps []byte
		var createdAt time.Time
		var embeddingText sql.NullString
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative, &concepts, &filesRead, &filesModified, &nextSteps,
			&r.CostUSD, &createdAt, &r.CreatedAtEpoch, &r.ContentHash, &embeddingText); err != nil {
			return nil, fmt.Errorf("scan export row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = jsonDecode(facts)
		r.Observation.Concepts = jsonDecode(concepts)
		r.Observation.FilesRead = jsonDecode(filesRead)
		r.Observation.FilesModified = jsonDecode(filesModified)
		r.Observation.NextSteps = jsonDecode(nextSteps)
		// CreatedAt travels as an RFC3339 string in ExportRow so both
		// backends' export files share one on-the-wire format regardless
		// of whether the source column is SQLite TEXT or Postgres
		// TIMESTAMPTZ — this is what lets export/import double as the
		// SQLite<->Postgres migration path, not just same-backend backup.
		r.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		if embeddingText.Valid {
			// pgvector.Vector.Scan only accepts []byte/string, not the raw
			// nil a SQL NULL embedding scans as for a row that was never
			// embedded — hence scanning into sql.NullString first and only
			// parsing when it's actually present.
			var v pgvector.Vector
			if err := v.Parse(embeddingText.String); err != nil {
				return nil, fmt.Errorf("parse embedding for observation %d: %w", r.ID, err)
			}
			r.Embedding = v.Slice()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ImportRow re-inserts a previously exported row, including its
// embedding if it had one — see sqlite.Store's ImportRow doc comment for
// the idempotency/timestamp/embedding-preservation rationale, identical
// here.
func (s *Store) ImportRow(ctx context.Context, row memory.ExportRow) (memory.InsertResult, error) { // memory.ParseExportCreatedAt, not an inline time.Parse: this backend
	// validated CreatedAt from the start while the SQLite one didn't, and
	// that silent disagreement was itself a real bug (see the helper's own
	// doc comment). Sharing one definition is what keeps them from drifting
	// apart again.
	createdAt, err := memory.ParseExportCreatedAt(row.CreatedAt)
	if err != nil {
		return memory.InsertResult{}, err
	}
	res, err := s.insertRow(ctx, row.SessionID, row.Project, row.ToolName, row.ContentHash,
		row.Observation, row.CostUSD, createdAt, row.CreatedAtEpoch)
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
