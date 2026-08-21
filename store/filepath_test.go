package store

import (
	"path/filepath"
	"testing"
)

// TestNormalizeFilePathMakesStorageCanonical is the regression test for a
// bug that made the PreToolUse file-context feature work only by luck.
//
// The hook looks up the path Claude Code puts in the tool payload, which
// is absolute. The worker stored whatever the observer model emitted,
// verbatim, and the prompt says only "<file>...</file>" — so the model
// sometimes shortened an absolute input to a repo-relative path. Those
// never match. Measured in this project's own development store: 8
// absolute and 7 relative paths recorded, and across the whole history of
// file-context.log only 2 successful injections against 9 "no prior
// observations". A real two-session soak reproduced it precisely.
func TestNormalizeFilePathMakesStorageCanonical(t *testing.T) {
	cwd := "/repo/src/auth"

	// The failing case: the model recorded a repo-relative path while the
	// hook queried the absolute one.
	if got := NormalizeFilePath(cwd, "tokens.go"); got != "/repo/src/auth/tokens.go" {
		t.Fatalf("NormalizeFilePath(%q, %q) = %q, want it resolved against the cwd", cwd, "tokens.go", got)
	}
	// Already absolute: cleaned, never re-rooted.
	if got := NormalizeFilePath(cwd, "/other/place.go"); got != "/other/place.go" {
		t.Fatalf("an absolute path was altered: %q", got)
	}
	if got := NormalizeFilePath(cwd, "/other/./sub/../place.go"); got != "/other/place.go" {
		t.Fatalf("an absolute path was not cleaned: %q", got)
	}
	// Two spellings of the same file must converge, or the index still
	// disagrees with itself.
	a := NormalizeFilePath(cwd, "tokens.go")
	b := NormalizeFilePath(cwd, "./tokens.go")
	c := NormalizeFilePath("/repo", "src/auth/tokens.go")
	if a != b || a != c {
		t.Fatalf("equivalent paths did not converge: %q / %q / %q", a, b, c)
	}
}

// TestNormalizeFilePathLeavesUnknownAlone pins the fail-safe direction: an
// empty cwd must not cause a guess. Resolving against the wrong directory
// would manufacture a confidently incorrect path, which is worse than
// leaving an imperfect one — the same reasoning the file-context staleness
// gate uses for an unstattable file.
func TestNormalizeFilePathLeavesUnknownAlone(t *testing.T) {
	if got := NormalizeFilePath("", "tokens.go"); got != "tokens.go" {
		t.Fatalf("NormalizeFilePath with no cwd = %q, want the input untouched", got)
	}
	if got := NormalizeFilePath("/repo", ""); got != "" {
		t.Fatalf("an empty path became %q", got)
	}
}

func TestNormalizeFilePathsHandlesLists(t *testing.T) {
	got := NormalizeFilePaths("/repo", []string{"a.go", "/abs/b.go", ""})
	want := []string{filepath.Join("/repo", "a.go"), "/abs/b.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v — empty entries must be dropped, not stored as \"\"", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if NormalizeFilePaths("/repo", nil) != nil {
		t.Fatal("nil in must stay nil out")
	}
}

// TestNormalizedPathsMakeTheLookupMatch is the end-to-end shape of the
// bug: store what a model would have emitted, then look it up the way the
// hook does.
func TestNormalizedPathsMakeTheLookupMatch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	cwd := "/repo/src/auth"
	abs := "/repo/src/auth/tokens.go"
	// The observer emitted a relative path; the write path normalizes it.
	obs := Observation{Type: "discovery", Title: "read tokens",
		FilesRead: NormalizeFilePaths(cwd, []string{"tokens.go"})}
	if _, err := st.Insert("s1", "repo", "Read", ContentHash("s1", "Read", "t", "1"), obs, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The hook queries the absolute path from the payload.
	got, err := st.ObservationsForFile("repo", abs, 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("looking up %q found %d observations, want 1 — this is exactly the mismatch that "+
			"made file-context silently find nothing", abs, len(got))
	}
}
