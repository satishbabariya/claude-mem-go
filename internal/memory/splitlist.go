package memory

import "strings"

// SplitCommaList splits a comma-separated argument into trimmed,
// non-empty parts — real claude-mem's own SearchManager.ts/
// SearchOrchestrator.ts do the identical split for its search tool's
// obs_type (and concepts/files) filters ("Comma-separated for multiple"),
// before SessionSearch.ts branches on array-vs-string to build an IN
// clause instead of a plain equality one. Returns nil for an empty
// input, matching "no filter," not a single empty-string filter.
func SplitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
