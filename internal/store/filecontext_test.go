package store

import (
	"fmt"
	"strings"
	"testing"
)

func fcObs(session, title string, read, modified []string) SearchResult {
	return SearchResult{
		SessionID:   session,
		Observation: Observation{Type: "change", Title: title, FilesRead: read, FilesModified: modified},
	}
}

// TestSelectFileContextRecoversOlderSessions is the regression test for a
// failure that defeats the product's own purpose. PostToolUse fires per
// tool call with matcher "*", so one session that reads, edits, re-reads
// and fixes a file produces five-plus observations about it — all recent.
// The hook took the top N by recency, so every injected slot went to the
// session whose contents Claude still has in context anyway, and the
// accumulated cross-session knowledge it CANNOT recover was crowded out.
func TestSelectFileContextRecoversOlderSessions(t *testing.T) {
	const target = "/proj/auth.go"
	// Newest first, as ObservationsForFile returns them.
	var candidates []SearchResult
	for i := 5; i >= 1; i-- {
		files := []string{target}
		if i == 3 {
			for j := 0; j < 40; j++ {
				files = append(files, fmt.Sprintf("/proj/other%d.go", j))
			}
		}
		candidates = append(candidates, fcObs("sess-today", fmt.Sprintf("today step %d", i), files, nil))
	}
	candidates = append(candidates,
		fcObs("sess-mar", "never log the raw token", []string{target}, nil),
		fcObs("sess-feb", "refresh path is NOT thread-safe", []string{target}, nil),
		fcObs("sess-jan", "tokens expire after 15m by design", []string{target}, nil),
	)

	got := SelectFileContext(candidates, target, 5)

	bySession := map[string]int{}
	for _, r := range got {
		bySession[r.SessionID]++
	}
	if bySession["sess-today"] > 1 {
		t.Fatalf("sess-today got %d of the slots, want at most 1 — a single session's repeated "+
			"touches must not displace other sessions", bySession["sess-today"])
	}
	for _, want := range []string{"sess-jan", "sess-feb", "sess-mar"} {
		if bySession[want] != 1 {
			t.Fatalf("%s is missing from the selection; these are exactly the facts Claude "+
				"cannot recover by reading the file. Got: %v", want, titles(got))
		}
	}
}

// TestSelectFileContextPrefersSpecificObservations pins the scoring: an
// observation that MODIFIED the file says more about it than one that
// merely read it, and one touching a handful of files says more about any
// single file than one that swept forty.
func TestSelectFileContextPrefersSpecificObservations(t *testing.T) {
	const target = "/proj/auth.go"
	sweeping := []string{target}
	for j := 0; j < 40; j++ {
		sweeping = append(sweeping, fmt.Sprintf("/proj/other%d.go", j))
	}
	candidates := []SearchResult{
		fcObs("s1", "swept 41 files", sweeping, nil),                  // newest, but vague
		fcObs("s2", "edited auth.go directly", nil, []string{target}), // older, but specific
	}

	got := SelectFileContext(candidates, target, 1)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Observation.Title != "edited auth.go directly" {
		t.Fatalf("selected %q, want the specific one — recency must not outrank a targeted "+
			"modification over a 41-file sweep", got[0].Observation.Title)
	}
}

// TestSelectFileContextKeepsRecencyAmongEquals is the counterweight to the
// scoring: specificity breaks ties, it does not replace recency. Two
// equally specific observations must still come back newest-first.
func TestSelectFileContextKeepsRecencyAmongEquals(t *testing.T) {
	const target = "/proj/auth.go"
	candidates := []SearchResult{
		fcObs("s1", "newest", []string{target}, nil),
		fcObs("s2", "middle", []string{target}, nil),
		fcObs("s3", "oldest", []string{target}, nil),
	}
	got := SelectFileContext(candidates, target, 3)
	if len(got) != 3 || got[0].Observation.Title != "newest" || got[2].Observation.Title != "oldest" {
		t.Fatalf("equal-specificity results were reordered: %v — the sort must be stable so "+
			"recency still decides among equals", titles(got))
	}
}

// TestSelectFileContextHandlesMissingSessionIDs guards a real collapse:
// keying dedup on an empty session id would fold every such observation
// into a single bucket and drop all but one.
func TestSelectFileContextHandlesMissingSessionIDs(t *testing.T) {
	const target = "/proj/auth.go"
	candidates := []SearchResult{
		{ID: 1, Observation: Observation{Title: "a", FilesRead: []string{target}}},
		{ID: 2, Observation: Observation{Title: "b", FilesRead: []string{target}}},
		{ID: 3, Observation: Observation{Title: "c", FilesRead: []string{target}}},
	}
	if got := SelectFileContext(candidates, target, 5); len(got) != 3 {
		t.Fatalf("got %d results from 3 session-less observations, want 3 — they must not "+
			"collapse into one bucket. Got: %v", len(got), titles(got))
	}
}

func TestFileContextCandidateLimitIsBounded(t *testing.T) {
	if got := FileContextCandidateLimit(5); got != 50 {
		t.Fatalf("FileContextCandidateLimit(5) = %d, want 50", got)
	}
	if got := FileContextCandidateLimit(100); got != 100 {
		t.Fatalf("FileContextCandidateLimit(100) = %d, want it capped at 100 so a pathological "+
			"file cannot pull an unbounded set", got)
	}
}

func titles(rs []SearchResult) string {
	var b strings.Builder
	for i, r := range rs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s/%s", r.SessionID, r.Observation.Title)
	}
	return b.String()
}
