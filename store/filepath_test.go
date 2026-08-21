package store

import (
	"os"
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

// TestNormalizeFilePathDisambiguatesByExistence covers a bug this
// function's FIRST version introduced, found by inspecting real rows
// after a soak rather than by any test.
//
// A relative path from the observer is ambiguous: the model is shown the
// raw tool input and asked for "<file>...</file>", so it emits sometimes
// a cwd-relative path ("tokens.go") and sometimes a repo-relative one
// ("src/auth/tokens.go"). Joining blindly against cwd turned the second
// kind into nonsense — with cwd=<repo>/src/auth a real soak produced
// ".../repo/src/auth/src/auth/tokens.go", a doubled path matching
// nothing, which is worse than the relative string it replaced.
//
// Existence on disk resolves it, and is available here because the path
// names a file the session just touched.
func TestNormalizeFilePathDisambiguatesByExistence(t *testing.T) {
	root := t.TempDir()
	// A real repo layout: a git marker at the root, the file two levels in.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	deep := filepath.Join(root, "src", "auth")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(deep, "tokens.go")
	if err := os.WriteFile(target, []byte("package auth\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Both spellings the model actually produces must converge on the one
	// real file, or the index disagrees with itself.
	if got := NormalizeFilePath(deep, "tokens.go"); got != target {
		t.Fatalf("cwd-relative %q resolved to %q, want %q", "tokens.go", got, target)
	}
	if got := NormalizeFilePath(deep, "src/auth/tokens.go"); got != target {
		t.Fatalf("repo-relative %q resolved to %q, want %q.\nJoining blindly against cwd is what "+
			"produced the doubled .../src/auth/src/auth/tokens.go seen in a real store.",
			"src/auth/tokens.go", got, target)
	}
}

// TestNormalizeFilePathFallsBackWhenNothingResolves pins the last resort.
// A path that names no existing file (deleted since, or never real) still
// has to become absolute: the hook looks up absolute paths, so leaving it
// relative guarantees no match, whereas the cwd reading is the likelier
// of the two.
func TestNormalizeFilePathFallsBackWhenNothingResolves(t *testing.T) {
	cwd := t.TempDir()
	want := filepath.Join(cwd, "gone", "deleted.go")
	if got := NormalizeFilePath(cwd, "gone/deleted.go"); got != want {
		t.Fatalf("got %q, want the cwd-join fallback %q", got, want)
	}
}

// TestNormalizeFilePathNeverPicksADirectory guards the existence check
// itself: a relative name that happens to match a DIRECTORY under cwd
// must not be treated as a resolved file, or a path like "src" would
// silently canonicalize to the directory rather than to whatever file was
// meant.
func TestNormalizeFilePathNeverPicksADirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "auth"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// "src/auth" exists as a directory under cwd; it is not a file, so the
	// existence check must reject it and fall through.
	got := NormalizeFilePath(root, "src/auth")
	if got != filepath.Join(root, "src", "auth") {
		t.Fatalf("got %q", got)
	}
}
