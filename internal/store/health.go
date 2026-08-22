package store

import (
	"fmt"
	"strings"
)

// HealthDetails returns backend-specific operational facts for `doctor` —
// details the rest of the Backend interface (Insert/Search/etc.) has no
// way to surface, because they're specific to how each backend actually
// runs, not what it stores. For SQLite: the PRAGMA settings actually in
// effect on this connection at runtime (journal_mode, foreign_keys,
// busy_timeout) — confirming the WAL/FK/busy-timeout fix (see store.go's
// sqliteDSNParams) genuinely took effect, not just that it was requested
// in the DSN. Keys are stable strings a caller looks up by name; iteration
// order isn't guaranteed.
func (s *Store) HealthDetails() (map[string]string, error) {
	details := map[string]string{}

	var journalMode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("query journal_mode: %w", err)
	}
	details["journal_mode"] = journalMode

	var foreignKeys int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return nil, fmt.Errorf("query foreign_keys: %w", err)
	}
	details["foreign_keys"] = fmt.Sprintf("%d", foreignKeys)

	var busyTimeoutMS int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeoutMS); err != nil {
		return nil, fmt.Errorf("query busy_timeout: %w", err)
	}
	details["busy_timeout_ms"] = fmt.Sprintf("%d", busyTimeoutMS)

	// embedding_dims: a real, silent-failure risk this schema permits that
	// Postgres's fixed-width vector(N) column type structurally can't —
	// observation_vectors.dims is recorded per row (SaveEmbedding just
	// uses len(vec)), so nothing stops two rows from holding
	// different-dimension vectors if the configured Ollama embedding model
	// ever changes. SemanticSearch's cosineSimilarity returns -1 (the
	// theoretical minimum) on any length mismatch rather than erroring, so
	// a dimension change doesn't fail loudly anywhere — the old
	// embeddings just quietly stop ever matching a query embedded with the
	// new model, forever, with nothing here or in the worker/hook logs
	// ever saying so. More than one distinct dimension present is exactly
	// that condition already having happened. Omitted entirely when there
	// are no embedded observations yet — nothing to report.
	rows, err := s.db.Query(`SELECT dims, COUNT(*) FROM observation_vectors GROUP BY dims ORDER BY dims`)
	if err != nil {
		return nil, fmt.Errorf("query embedding dims histogram: %w", err)
	}
	var histogram []string
	for rows.Next() {
		var dims, count int
		if err := rows.Scan(&dims, &count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan embedding dims histogram: %w", err)
		}
		histogram = append(histogram, fmt.Sprintf("%d:%d", dims, count))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query embedding dims histogram: %w", err)
	}
	rows.Close()
	if len(histogram) > 0 {
		details["embedding_dims"] = strings.Join(histogram, ",")
		details["embedding_dims_consistent"] = fmt.Sprintf("%t", len(histogram) == 1)
	}

	return details, nil
}
