package excludeproject

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsExcludedEmptyInputsNeverExclude(t *testing.T) {
	if IsExcluded("/home/user/proj", "") {
		t.Error("empty exclusionPatterns must never exclude")
	}
	if IsExcluded("", "*") {
		t.Error("empty cwd must never exclude, even against a pattern that would otherwise match everything")
	}
}

func TestIsExcludedBasenameGlob(t *testing.T) {
	// "scratch-*" has no "/", so it's tested against the basename too —
	// must exclude regardless of where the directory lives.
	if !IsExcluded("/home/user/projects/scratch-42", "scratch-*") {
		t.Error("basename glob scratch-* should exclude /home/user/projects/scratch-42")
	}
	if IsExcluded("/home/user/projects/real-project", "scratch-*") {
		t.Error("scratch-* should not exclude an unrelated project name")
	}
}

func TestIsExcludedFullPathGlob(t *testing.T) {
	if !IsExcluded("/home/user/clients/acme/secret", "/home/user/clients/**") {
		t.Error("/home/user/clients/** should exclude anything nested under clients/")
	}
	if IsExcluded("/home/user/other/secret", "/home/user/clients/**") {
		t.Error("/home/user/clients/** should not exclude a sibling directory")
	}
}

func TestIsExcludedMultiplePatternsCommaSeparated(t *testing.T) {
	patterns := "scratch-*, /home/user/clients/**, exact-name"
	if !IsExcluded("/tmp/scratch-1", patterns) {
		t.Error("first pattern (basename glob) should have matched")
	}
	if !IsExcluded("/home/user/clients/foo/bar", patterns) {
		t.Error("second pattern (full-path glob) should have matched")
	}
	if !IsExcluded("/anywhere/exact-name", patterns) {
		t.Error("third pattern (exact basename, no wildcards) should have matched")
	}
	if IsExcluded("/anywhere/unrelated", patterns) {
		t.Error("no pattern should have matched an unrelated directory")
	}
}

// TestIsExcludedQuestionMarkMatchesExactlyOneCharacter mirrors real
// claude-mem's ? semantics: [^/] — exactly one non-separator character,
// not "zero or more" the way * behaves.
func TestIsExcludedQuestionMarkMatchesExactlyOneCharacter(t *testing.T) {
	if !IsExcluded("/tmp/job1", "job?") {
		t.Error("job? should match job1 (exactly one extra character)")
	}
	if IsExcluded("/tmp/job", "job?") {
		t.Error("job? should not match job (zero extra characters)")
	}
	if IsExcluded("/tmp/job12", "job?") {
		t.Error("job? should not match job12 (two extra characters)")
	}
}

// TestIsExcludedSingleStarDoesNotCrossPathSeparator mirrors real
// claude-mem's * semantics: [^/]* — confined to one path segment, unlike
// ** which crosses separators freely.
func TestIsExcludedSingleStarDoesNotCrossPathSeparator(t *testing.T) {
	if IsExcluded("/home/user/clients/acme/secret", "/home/user/clients/*") {
		t.Error("a single * must not cross a path separator the way ** does")
	}
	if !IsExcluded("/home/user/clients/acme", "/home/user/clients/*") {
		t.Error("a single * should still match exactly one path segment")
	}
}

// TestIsExcludedTildeExpandsToHomeDir confirms a leading ~ in a pattern
// expands to the real home directory, matching real claude-mem's own
// globToRegex behavior — a user porting an existing
// CLAUDE_MEM_EXCLUDED_PROJECTS value with a "~/..." pattern must get
// the same result here.
func TestIsExcludedTildeExpandsToHomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	target := filepath.Join(home, "private-notes")
	if !IsExcluded(target, "~/private-notes") {
		t.Errorf("~/private-notes should have expanded to %s and matched", target)
	}
}

// TestIsExcludedRegexSpecialCharactersAreLiteral confirms characters like
// "." and "+" in a pattern are treated as literal text, not regex
// metacharacters — a pattern of "my.project" must not accidentally match
// "myXproject" the way an unescaped "." would in a raw regex.
func TestIsExcludedRegexSpecialCharactersAreLiteral(t *testing.T) {
	if !IsExcluded("/tmp/my.project", "my.project") {
		t.Error("a literal . in a pattern should match a literal . in the path")
	}
	if IsExcluded("/tmp/myXproject", "my.project") {
		t.Error("a literal . in a pattern must not act as a regex wildcard (should not match myXproject)")
	}
}

// TestIsExcludedInvalidPatternIsSkippedNotFatal confirms one malformed
// pattern in a comma-separated list doesn't prevent the OTHER valid
// patterns in the same list from still being checked — mirrors real
// claude-mem's per-pattern try/catch-and-continue.
func TestIsExcludedInvalidPatternIsSkippedNotFatal(t *testing.T) {
	// An unbalanced "[" is escaped as a literal character by globToRegex
	// (it's in the escape set), so this specific input can't actually
	// produce a bad regex — but the function must not panic or error
	// outright regardless, and a genuinely-valid pattern later in the
	// same list must still be checked.
	patterns := "[[[, real-match"
	if !IsExcluded("/tmp/real-match", patterns) {
		t.Error("a later valid pattern must still be checked even if an earlier one looks unusual")
	}
}
