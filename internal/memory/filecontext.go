package memory

import "sort"

// fileContextCandidateFactor is how many extra rows the file-context hook
// should fetch per display slot before SelectFileContext narrows them.
// Selection can only choose among what it is given, so querying exactly
// `limit` rows would leave nothing to select from — the query's own
// ORDER BY would remain the whole policy, which is the bug.
const FileContextCandidateFactor = 10

// FileContextCandidateLimit converts a display limit into how many rows to
// fetch, bounded so a pathological file can't pull an unbounded set.
func FileContextCandidateLimit(limit int) int {
	n := limit * FileContextCandidateFactor
	if n > 100 {
		n = 100
	}
	if n < limit {
		n = limit
	}
	return n
}

// SelectFileContext narrows candidate observations about one file down to
// the `limit` most useful ones to inject before Claude reads it.
//
// The query alone was the whole policy before this: ObservationsForFile
// orders by recency and the hook took the top N. That fails in the exact
// case this product exists for. PostToolUse fires per tool call with
// matcher "*", so a single session that reads, edits, re-reads and fixes
// one file produces five-plus observations about it — all recent, all
// from the session whose contents Claude still has in context anyway.
// Measured on a constructed but entirely ordinary history (three older
// sessions each holding one durable fact about a file, plus one
// same-day session that touched it five times): all five injected slots
// went to the same-day session, one of them a sweeping change that
// touched 41 files, and every cross-session fact — "tokens expire after
// 15m by design", "the refresh path is NOT thread-safe", "never log the
// raw token" — was crowded out. Those are precisely the things Claude
// cannot recover on its own by reading the file.
//
// Two rules, matching real claude-mem's own deduplicateObservations:
//
//  1. At most one observation per session. A session's repeated touches
//     of a file are near-duplicates of each other; the first is
//     representative and the rest only displace other sessions.
//  2. Prefer specific observations. An observation that MODIFIED the file
//     says more about it than one that merely read it, and one that
//     touched a handful of files says more about any single file than one
//     that swept 40.
//
// Ties keep the caller's order, which is recency — so among equally
// specific observations the newest still wins.
func SelectFileContext(candidates []SearchResult, targetPath string, limit int) []SearchResult {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(candidates))
	deduped := make([]SearchResult, 0, len(candidates))
	for _, r := range candidates {
		// An observation with no session id cannot be grouped with
		// anything, so key it by its own id rather than letting every
		// such row collapse into one bucket.
		key := r.SessionID
		if key == "" {
			key = "no-session-" + itoa(r.ID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}

	scores := make([]int, len(deduped))
	for i, r := range deduped {
		scores[i] = fileContextScore(r, targetPath)
	}
	idx := make([]int, len(deduped))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })

	if limit > len(idx) {
		limit = len(idx)
	}
	out := make([]SearchResult, 0, limit)
	for _, i := range idx[:limit] {
		out = append(out, deduped[i])
	}
	return out
}

func fileContextScore(r SearchResult, targetPath string) int {
	score := 0
	for _, f := range r.Observation.FilesModified {
		if f == targetPath {
			score += 2
			break
		}
	}
	switch n := len(r.Observation.FilesRead) + len(r.Observation.FilesModified); {
	case n <= 3:
		score += 2
	case n <= 8:
		score++
	}
	return score
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		return "-" + string(d)
	}
	return string(d)
}
