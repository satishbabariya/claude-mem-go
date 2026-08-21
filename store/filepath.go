package store

import "path/filepath"

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
	return filepath.Join(cwd, path)
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
