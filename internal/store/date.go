package store

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
