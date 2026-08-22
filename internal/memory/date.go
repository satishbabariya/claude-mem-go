package memory

import (
	"fmt"
	"strconv"
	"time"
)

// ParseDateArg parses a date-range argument for Search's dateStartMs/
// dateEndMs — real claude-mem's own dateStart/dateEnd search filters take
// an ISO date string and convert it with JS's `new Date(s).getTime()`
// (SessionSearch.ts), which accepts both a full RFC3339 timestamp and a
// bare "YYYY-MM-DD" date (treated as UTC midnight). This mirrors both
// forms, plus a plain integer epoch-milliseconds string for a caller that
// already has one. An empty string returns 0 (unbounded), never an
// error, since that's the common case (a caller passing only one side of
// the range, or neither).
func ParseDateArg(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return ms, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().UnixMilli(), nil
	}
	return 0, fmt.Errorf("unrecognized date %q: want RFC3339 (2025-01-02T15:04:05Z), a bare date (2025-01-02), or epoch milliseconds", s)
}

// ParseExportCreatedAt validates an ExportRow's CreatedAt and returns it
// parsed. Both backends' ImportRow call this, so there is exactly one
// definition of what a valid timestamp is — which is the actual point,
// not a convenience: the two used to disagree, and that disagreement was
// itself the bug.
//
// The Postgres backend has always parsed CreatedAt (its column is
// TIMESTAMPTZ, so it had no choice), while the SQLite backend passed the
// string straight into a TEXT column that accepts anything at all. A
// corrupt or hand-edited backup therefore imported "successfully" into
// SQLite — reported as `Imported 3 observation(s)` with no warning —
// while writing 'not-a-date' or an empty string into created_at.
//
// The damage isn't confined to one poisoned column, which is what makes
// this worth a hard error rather than a lenient coercion: ExportAll
// re-emits the bad value verbatim, so the SQLite→Postgres migration path
// this project documents then fails on it, and cmdImport has no
// transaction, so that failure lands half-restored. A restore that
// reported success quietly renders the store un-migratable, and nobody
// finds out until cutover. Measured end to end, not theorized.
//
// Callers keep storing the ORIGINAL string rather than a re-formatted
// one, so well-formed rows round-trip byte-identically and a repeated
// import stays a genuine no-op.
func ParseExportCreatedAt(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse CreatedAt %q: %w", s, err)
	}
	return t, nil
}
