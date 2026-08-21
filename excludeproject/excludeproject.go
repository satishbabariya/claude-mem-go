// Package excludeproject lets a user opt specific projects out of
// automatic capture — a real feature gap this port had until now, not
// present anywhere in claude-mem-go despite being a real claude-mem
// capability every automatic hook handler checks
// (src/shared/should-track-project.ts's shouldTrackProject, backed by
// src/utils/project-filter.ts's isProjectExcluded). Without it, the only
// way to keep a sensitive/client-confidential/scratch project out of a
// shared memory database was to not install this plugin at all —
// all-or-nothing, unlike real claude-mem's per-project opt-out.
//
// Deliberately mirrors real claude-mem's exact glob semantics
// (isProjectExcluded/globToRegex) rather than inventing new ones: a user
// migrating an existing CLAUDE_MEM_EXCLUDED_PROJECTS value should get
// identical matching behavior here, not a subtly different dialect.
package excludeproject

import (
	"os"
	"path"
	"regexp"
	"strings"
)

// globSpecialChars are the regex metacharacters real claude-mem's own
// globToRegex escapes before applying glob substitutions — deliberately
// NOT including `*` or `?`, which are handled separately below as glob
// wildcards rather than literal characters.
const globSpecialChars = `.+^${}()|[]\`

// globstarPlaceholder stands in for a literal "**" between the escape
// pass and the final substitution — same two-step reason real claude-mem's
// version uses one (a bare regex replace of "*" would also rewrite the
// two characters making up an already-converted "**", corrupting it, if
// done in the wrong order without a placeholder).
const globstarPlaceholder = "\x00GLOBSTAR\x00"

// globToRegex turns one glob pattern into an anchored regular expression,
// matching real claude-mem's own glob dialect exactly: a leading `~`
// expands to the home directory, backslashes normalize to forward
// slashes, everything else regex-special is escaped, then `**` becomes
// `.*`, a single `*` becomes `[^/]*` (matches within one path segment,
// not across `/`), and `?` becomes `[^/]` (exactly one non-separator
// character).
func globToRegex(pattern string) (*regexp.Regexp, error) {
	expanded := pattern
	if strings.HasPrefix(pattern, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = home + pattern[1:]
		}
	}
	expanded = strings.ReplaceAll(expanded, `\`, `/`)

	var escaped strings.Builder
	for _, r := range expanded {
		if strings.ContainsRune(globSpecialChars, r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}

	result := escaped.String()
	result = strings.ReplaceAll(result, "**", globstarPlaceholder)
	result = strings.ReplaceAll(result, "*", `[^/]*`)
	result = strings.ReplaceAll(result, "?", `[^/]`)
	result = strings.ReplaceAll(result, globstarPlaceholder, ".*")

	return regexp.Compile("^" + result + "$")
}

// IsExcluded reports whether cwd matches any pattern in the
// comma-separated exclusionPatterns list. Matches against both the full
// (forward-slash-normalized) path and its basename — a pattern like
// "scratch-*" excludes any project directory whose NAME matches
// regardless of where it lives, while a pattern containing "/" anchors
// to the full path — mirroring real claude-mem's own isProjectExcluded
// exactly. An empty exclusionPatterns or empty cwd is never excluded. An
// individual pattern that fails to compile (regexp.Compile can still
// fail even on this restricted dialect, e.g. an unbalanced bracket
// somewhere in the literal, non-glob portion of a pattern) is skipped
// rather than treated as excluding everything or erroring the whole
// call, matching real claude-mem's own try/catch-and-continue per pattern.
func IsExcluded(cwd, exclusionPatterns string) bool {
	if strings.TrimSpace(exclusionPatterns) == "" || cwd == "" {
		return false
	}
	normalized := strings.ReplaceAll(cwd, `\`, `/`)
	base := path.Base(normalized)

	for _, raw := range strings.Split(exclusionPatterns, ",") {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		re, err := globToRegex(pattern)
		if err != nil {
			continue
		}
		if re.MatchString(normalized) || re.MatchString(base) {
			return true
		}
	}
	return false
}
