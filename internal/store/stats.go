package store

import "database/sql"

// Stats implements Backend.Stats for SQLite.
//
// Three queries rather than one: the per-type breakdown is a GROUP BY,
// and embedding counts live in a separate table (observation_vectors),
// so joining them into a single statement would trade clarity for
// nothing measurable — this runs on demand from `stats` and `doctor`,
// not on any hot path.
func (s *Store) Stats() (StoreStats, error) {
	var out StoreStats
	out.ByType = map[string]int{}

	var oldest, newest sql.NullInt64
	err := s.db.QueryRow(`
		SELECT count(*), count(DISTINCT project), count(DISTINCT session_id),
		       min(created_at_epoch), max(created_at_epoch)
		FROM observations`).Scan(&out.Observations, &out.Projects, &out.Sessions, &oldest, &newest)
	if err != nil {
		return StoreStats{}, err
	}
	// min()/max() over no rows is SQL NULL, and scanning that straight
	// into an int64 is a hard error ("converting NULL to int64 is
	// unsupported") — so sql.NullInt64 is what actually makes an empty
	// store work here, not the .Valid checks below, which are
	// belt-and-braces: NullInt64.Int64 is already 0 when invalid.
	// Confirmed by break/restore in both directions.
	if oldest.Valid {
		out.OldestEpochMs = oldest.Int64
	}
	if newest.Valid {
		out.NewestEpochMs = newest.Int64
	}

	rows, err := s.db.Query(`SELECT type, count(*) FROM observations GROUP BY type`)
	if err != nil {
		return StoreStats{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return StoreStats{}, err
		}
		out.ByType[t] = n
	}
	if err := rows.Err(); err != nil {
		return StoreStats{}, err
	}

	if err := s.db.QueryRow(`SELECT count(*) FROM observation_vectors`).Scan(&out.Embedded); err != nil {
		return StoreStats{}, err
	}
	return out, nil
}
