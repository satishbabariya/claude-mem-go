// Semantic search over observations, via embeddings stored as BLOBs and
// scored by brute-force cosine similarity in Go. This is claude-mem's real
// Chroma sync's analog, minus Chroma: no ANN index, no external vector
// service — just every embedding loaded into memory and compared linearly.
//
// That's a real, honest limitation, not swept under the rug: brute force
// is O(n) per query and fine at the scale a single project's observations
// realistically reach (thousands, not millions), but it will not scale the
// way a real ANN index (Chroma, or a proper vector index) would. Treat this
// as "semantic search works," not "semantic search at scale."
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

const createVectorTableSQL = `
CREATE TABLE IF NOT EXISTS observation_vectors (
	observation_id INTEGER PRIMARY KEY REFERENCES observations(id) ON DELETE CASCADE,
	dims           INTEGER NOT NULL,
	embedding      BLOB NOT NULL
);
`

// encodeVector serializes a []float32 as little-endian bytes — simple and
// portable; not trying to match any on-disk format beyond this package's
// own read path.
func encodeVector(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func decodeVector(b []byte, dims int) ([]float32, error) {
	if len(b) != dims*4 {
		return nil, fmt.Errorf("embedding blob length %d does not match dims %d (want %d bytes)", len(b), dims, dims*4)
	}
	v := make([]float32, dims)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// SaveEmbedding stores vec for an already-persisted observation.
func (s *Store) SaveEmbedding(observationID int64, vec []float32) error {
	// observation_vectors is guaranteed to exist by Open's migrations
	// (see store/migrations.go) — no need to create it lazily here.
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO observation_vectors (observation_id, dims, embedding) VALUES (?, ?, ?)`,
		observationID, len(vec), encodeVector(vec),
	)
	if err != nil {
		return fmt.Errorf("save embedding for observation %d: %w", observationID, err)
	}
	return nil
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// VectorMatch is one semantic search result.
type VectorMatch struct {
	ID          int64
	Project     string
	ToolName    string
	Observation Observation
	Score       float64 // cosine similarity, [-1, 1], higher = closer
}

// SemanticSearch scores every embedded observation against queryVec by
// cosine similarity and returns the top `limit`, best match first.
//
// project scopes the comparison set to one project when non-empty, for the
// same cross-project-leak reason as Search (this store is one shared
// database across every project ever recorded on the machine).
func (s *Store) SemanticSearch(project string, queryVec []float32, limit int) ([]VectorMatch, error) {
	// observation_vectors is guaranteed to exist by Open's migrations
	// (see store/migrations.go) — no need to create it lazily here.
	query := `
		SELECT v.observation_id, v.dims, v.embedding,
		       o.project, o.tool_name, o.type, o.title, o.subtitle,
		       o.facts, o.narrative, o.concepts, o.files_read, o.files_modified
		FROM observation_vectors v
		JOIN observations o ON o.id = v.observation_id
	`
	var rows *sql.Rows
	var err error
	if project != "" {
		rows, err = s.db.Query(query+" WHERE o.project = ?", project)
	} else {
		rows, err = s.db.Query(query)
	}
	if err != nil {
		return nil, fmt.Errorf("query embeddings: %w", err)
	}
	defer rows.Close()

	var all []VectorMatch
	for rows.Next() {
		var dims int
		var blob []byte
		var facts, concepts, filesRead, filesModified string
		var nf nullableTextFields
		var m VectorMatch
		if err := rows.Scan(&m.ID, &dims, &blob,
			&m.Project, &m.ToolName, &m.Observation.Type, &nf.title, &nf.subtitle,
			&facts, &nf.narrative, &concepts, &filesRead, &filesModified); err != nil {
			return nil, fmt.Errorf("scan embedding row: %w", err)
		}
		nf.apply(&m.Observation)
		vec, err := decodeVector(blob, dims)
		if err != nil {
			return nil, err
		}
		m.Observation.Facts = parseJSONArray(facts)
		m.Observation.Concepts = parseJSONArray(concepts)
		m.Observation.FilesRead = parseJSONArray(filesRead)
		m.Observation.FilesModified = parseJSONArray(filesModified)
		m.Score = cosineSimilarity(queryVec, vec)
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
