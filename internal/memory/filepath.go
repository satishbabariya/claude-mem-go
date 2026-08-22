package memory

import (
	"os"
	"path/filepath"
)

// NormalizeFilePath makes an observation's file path canonical: absolute
// and cleaned, resolved against the working directory it was observed in.
//
// Storage was not canonical before this, and the consequence was that the
// PreToolUse file-context feature worked only by luck. The hook queries
// with the path Claude Code puts in the tool payload, which is absolute.
// The worker stored whatever the observer model emitted, verbatim, and
// the prompt says only "<file>...</file>" — so the model sometimes
// shortened an absolute input to a repo-relative one. Those never match,
// and the lookup silently returns nothing.
//
// Measured in this project's own development store: **8 absolute and 7
// relative** paths recorded, and across the entire history of
// file-context.log only 2 successful injections against 9 "no prior
// observations". A real two-session soak reproduced it exactly — session
// one recorded "src/auth/tokens.go" while session two looked up
// "/private/.../repo/src/auth/tokens.go" and found nothing, despite the
// observation being right there.
//
// Normalizing on the WRITE path rather than at query time is what makes
// this durable: one canonical form in the column means every reader
// agrees, including the SQLite observation_files index (whose trigger
// derives from this same JSON) and Postgres's GIN indexes on it.
//
// A path that is already absolute is only cleaned. An empty cwd leaves
// the path untouched rather than guessing — resolving against a wrong
// directory would manufacture a confidently incorrect path, which is
// worse than leaving an imperfect one.
func NormalizeFilePath(cwd, path string) string {
	if path == "" {
		return path
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if cwd == "" {
		return path
	}

	// A relative path from the observer is AMBIGUOUS, and the first
	// version of this function guessed wrong. The model is shown the raw
	// tool input and asked for "<file>...</file>", so it emits sometimes
	// a cwd-relative path ("tokens.go") and sometimes a repo-relative one
	// ("src/auth/tokens.go"). Joining blindly against cwd turned the
	// second kind into nonsense: with cwd=<repo>/src/auth, a real soak
	// produced ".../repo/src/auth/src/auth/tokens.go" — a doubled path
	// that matches nothing and is worse than the relative string it
	// replaced.
	//
	// Existence on disk is the disambiguator, and it is available: this
	// path names a file the session just touched. Try each plausible base
	// and take the one that actually resolves.
	if p := filepath.Join(cwd, path); fileExists(p) {
		return p
	}
	if root, _, ok := findGitRoot(cwd); ok {
		if p := filepath.Join(root, path); fileExists(p) {
			return p
		}
	}
	// Neither resolved — the file may have been deleted since, or this is
	// not a real path at all. Fall back to the cwd join rather than
	// leaving it relative: the hook looks up absolute paths, so a
	// relative row is guaranteed not to match, whereas a cwd-relative
	// guess is the likelier of the two readings.
	return filepath.Join(cwd, path)
}

func fileExists(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && !fi.IsDir()
}

// NormalizeFilePaths applies NormalizeFilePath to a whole list, returning
// a new slice. Nil in, nil out — the column's default is an empty JSON
// array and callers distinguish "no files" from "one empty string".
func NormalizeFilePaths(cwd string, paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if n := NormalizeFilePath(cwd, p); n != "" {
			out = append(out, n)
		}
	}
	return out
}
