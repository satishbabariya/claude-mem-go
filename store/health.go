package store

import "fmt"

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

	return details, nil
}
