package store

import (
	"database/sql"
	"fmt"
)

// ExportRow is one observation as read back by ExportAll — everything
// needed to reinsert it via ImportRow, preserving its original identity
// (ContentHash, the idempotency key), timing (CreatedAt/CreatedAtEpoch),
// and — a real gap in this feature's first version, found by the same
// "does every write path do what the others do" check that caught the
// add_observation/Stop embedding bugs — its embedding, if it had one.
// Without this, export+import (backup, or the SQLite<->Postgres migration
// path) silently dropped semantic searchability for every single
// observation: a "migrate to Postgres for real ANN search at scale" would
// have arrived with nothing left to search.
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
	// Embedding is nil when the observation was never embedded (no embed
	// model configured at capture time, or the embedding call failed) —
	// that's a legitimate, common state, not an error.
	Embedding []float32 `json:"embedding,omitempty"`
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
	limit = clampNegativeLimit(limit)
	// LEFT JOIN, not INNER: most observations have no row in
	// observation_vectors at all (never embedded), and that must not
	// exclude them from the export.
	rows, err := s.db.Query(`
		SELECT o.id, o.session_id, o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified,
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

	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		var nf nullableTextFields
		var facts, concepts, filesRead, filesModified string
		var dims sql.NullInt64
		var embeddingBlob []byte
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.ToolName, &r.Observation.Type,
			&nf.title, &nf.subtitle, &facts, &nf.narrative, &concepts, &filesRead, &filesModified,
			&r.CostUSD, &r.CreatedAt, &r.CreatedAtEpoch, &r.ContentHash,
			&dims, &embeddingBlob); err != nil {
			return nil, fmt.Errorf("scan export row: %w", err)
		}
		nf.apply(&r.Observation)
		r.Observation.Facts = parseJSONArray(facts)
		r.Observation.Concepts = parseJSONArray(concepts)
		r.Observation.FilesRead = parseJSONArray(filesRead)
		r.Observation.FilesModified = parseJSONArray(filesModified)
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
// ContentHash (the same idempotent-dedup guarantee Insert provides — a
// row already imported is a no-op, not a duplicate), its original
// CreatedAt/CreatedAtEpoch, and its embedding, if it had one — a restore
// should be a full restore, not one that quietly leaves every observation
// unsearchable by meaning.
func (s *Store) ImportRow(row ExportRow) (InsertResult, error) {
	res, err := s.insertRow(row.SessionID, row.Project, row.ToolName, row.ContentHash,
		row.Observation, row.CostUSD, row.CreatedAt, row.CreatedAtEpoch)
	if err != nil {
		return res, err
	}
	if res.Inserted && len(row.Embedding) > 0 {
		if err := s.SaveEmbedding(res.ID, row.Embedding); err != nil {
			return res, fmt.Errorf("import observation %d: save embedding: %w", res.ID, err)
		}
	}
	return res, nil
}
