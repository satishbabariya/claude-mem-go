package store

import (
	"os"
	"path/filepath"
	"strings"
)

// ProjectContext is how a working directory maps onto memory. Primary is
// the project new observations are written under; AllProjects is what a
// read should span, which for a git worktree is more than one.
type ProjectContext struct {
	Primary string
	// Parent is the containing repository's project name when Primary
	// describes a git worktree, and empty otherwise.
	Parent     string
	IsWorktree bool
	// AllProjects always contains Primary, and for a worktree also the
	// parent — a worktree is a branch of the same work, so a session
	// started in one should still see what the repository already knows.
	AllProjects []string
}

// maxGitWalkDepth bounds the upward search for a repository root. Deep
// enough for any real checkout, and a hard stop rather than trusting the
// filesystem root check alone — a hook must not be able to walk forever
// on a pathological path or a mount loop.
const maxGitWalkDepth = 64

// ProjectContextFor derives the project identity for a working directory.
//
// This replaced a plain filepath.Base(cwd), duplicated at six call sites,
// which had two distinct failure modes and no way to notice either:
//
//   - FRAGMENTATION. A session started in a subdirectory got a different
//     project than one started at the repository root. Measured on one
//     real git repo: cwd=<repo> gave "gitproj", cwd=<repo>/src gave
//     "src", cwd=<repo>/src/auth gave "auth" — three projects for one
//     codebase, so nothing recorded in one was ever recalled in another.
//     Working in a subpackage is completely ordinary, which made this the
//     common case rather than an edge case.
//   - COLLISION, which is worse. Those fragment names are generic:
//     EVERY repository with a src/ directory shared the single project
//     "src". That is not just memory going missing, it is unrelated
//     projects' memory being mixed together and injected into each
//     other's sessions.
//
// Resolving the repository root fixes both: every directory inside one
// checkout maps to one stable, specific name. Real claude-mem does the
// same (getProjectContext -> findGitRepoRoot, and its #3262 note gives
// the identical reason: "sessions started in a subdirectory still get the
// parent/worktree compound key").
//
// Falls back to the old basename behaviour outside a repository, which is
// the only sensible answer there and keeps non-git directories working
// exactly as before.
func ProjectContextFor(cwd string) ProjectContext {
	if cwd == "" {
		return ProjectContext{Primary: "", AllProjects: nil}
	}
	base := filepath.Base(cwd)

	root, gitPath, ok := findGitRoot(cwd)
	if !ok {
		return ProjectContext{Primary: base, AllProjects: []string{base}}
	}

	rootName := filepath.Base(root)
	if parent, isWorktree := worktreeParent(gitPath); isWorktree && parent != "" {
		// Matches real claude-mem's composite key exactly: parent/worktree.
		// The parent is included in AllProjects because a worktree is a
		// branch of the same work — a session in it should still see what
		// the repository already knows.
		composite := parent + "/" + rootName
		return ProjectContext{
			Primary:     composite,
			Parent:      parent,
			IsWorktree:  true,
			AllProjects: []string{parent, composite},
		}
	}
	return ProjectContext{Primary: rootName, AllProjects: []string{rootName}}
}

// ProjectFor is ProjectContextFor(cwd).Primary — the write path, where
// exactly one name is needed.
func ProjectFor(cwd string) string { return ProjectContextFor(cwd).Primary }

// findGitRoot walks up from dir looking for a .git entry, returning the
// directory containing it and that entry's path.
//
// Deliberately a filesystem walk rather than shelling out to `git`: this
// runs in every hook on every tool call, a subprocess per invocation
// would be a real cost, and git is not guaranteed to be installed.
func findGitRoot(dir string) (root, gitPath string, ok bool) {
	dir = filepath.Clean(dir)
	for i := 0; i < maxGitWalkDepth; i++ {
		candidate := filepath.Join(dir, ".git")
		if _, err := os.Lstat(candidate); err == nil {
			return dir, candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root
		}
		dir = parent
	}
	return "", "", false
}

// worktreeParent reports the parent repository's project name when
// gitPath is a git worktree's .git FILE rather than a directory.
//
// Verified against a real `git worktree add`: the main repository's .git
// is a directory, while a worktree's is a file whose sole content is
// "gitdir: /path/to/parent/.git/worktrees/<name>". The parent repository
// is therefore everything before "/.git/worktrees/".
func worktreeParent(gitPath string) (string, bool) {
	fi, err := os.Lstat(gitPath)
	if err != nil || fi.IsDir() {
		return "", false
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	marker := string(filepath.Separator) + ".git" + string(filepath.Separator) + "worktrees" + string(filepath.Separator)
	idx := strings.Index(gitDir, marker)
	if idx < 0 {
		// A .git file that is not a worktree pointer — a submodule, for
		// instance. Treated as an ordinary repository rather than guessed
		// at, since a wrong parent name would silently merge two projects.
		return "", false
	}
	return filepath.Base(gitDir[:idx]), true
}
