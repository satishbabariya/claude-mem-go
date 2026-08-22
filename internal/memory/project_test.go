package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo makes a real git repository, not a hand-faked .git layout — the
// point of these tests is that the real on-disk shapes git produces are
// handled, and a fixture that only matches what the code expects would
// prove nothing.
func gitRepo(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", ".")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	return root
}

// TestProjectForResolvesTheRepoRoot is the regression test for the more
// common of two failure modes. filepath.Base(cwd) gave a different
// project for every subdirectory of one repository, so a session started
// in a subpackage — entirely ordinary — recalled nothing recorded at the
// root and vice versa.
func TestProjectForResolvesTheRepoRoot(t *testing.T) {
	root := gitRepo(t, "myrepo")
	sub := filepath.Join(root, "src", "auth")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for _, cwd := range []string{root, filepath.Join(root, "src"), sub} {
		if got := ProjectFor(cwd); got != "myrepo" {
			t.Fatalf("ProjectFor(%s) = %q, want %q — every directory in one checkout must map "+
				"to one project, or memory fragments across the same codebase", cwd, got, "myrepo")
		}
	}
}

// TestProjectForDoesNotCollideOnGenericSubdirNames covers the second and
// worse failure mode. The old behaviour named the project after the
// subdirectory, so every repository with a src/ shared the single project
// "src" — not memory going missing, but unrelated projects' memory being
// mixed together and injected into each other's sessions.
func TestProjectForDoesNotCollideOnGenericSubdirNames(t *testing.T) {
	a := gitRepo(t, "alpha")
	b := gitRepo(t, "beta")
	for _, r := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(r, "src"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	pa := ProjectFor(filepath.Join(a, "src"))
	pb := ProjectFor(filepath.Join(b, "src"))
	if pa == pb {
		t.Fatalf("two unrelated repositories both resolved to %q — their memory would be "+
			"pooled and cross-injected", pa)
	}
	if pa != "alpha" || pb != "beta" {
		t.Fatalf("got %q and %q, want alpha and beta", pa, pb)
	}
}

// TestProjectContextForRealWorktree runs against an actual `git worktree
// add`, because the shape it produces is the whole thing under test: the
// main repo's .git is a directory, a worktree's is a FILE containing
// "gitdir: <parent>/.git/worktrees/<name>".
func TestProjectContextForRealWorktree(t *testing.T) {
	root := gitRepo(t, "mainrepo")
	wt := filepath.Join(filepath.Dir(root), "wt-feature")
	cmd := exec.Command("git", "worktree", "add", "-q", wt, "-b", "feature")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable here: %v\n%s", err, out)
	}

	pc := ProjectContextFor(wt)
	if !pc.IsWorktree {
		t.Fatalf("IsWorktree = false for a real worktree (primary=%q)", pc.Primary)
	}
	if pc.Primary != "mainrepo/wt-feature" {
		t.Fatalf("Primary = %q, want %q", pc.Primary, "mainrepo/wt-feature")
	}
	if pc.Parent != "mainrepo" {
		t.Fatalf("Parent = %q, want %q", pc.Parent, "mainrepo")
	}
	// The parent must be readable from the worktree: a worktree is a
	// branch of the same work, so what the repository already knows
	// should still surface.
	if len(pc.AllProjects) != 2 || pc.AllProjects[0] != "mainrepo" {
		t.Fatalf("AllProjects = %v, want [mainrepo mainrepo/wt-feature]", pc.AllProjects)
	}

	// A subdirectory of the worktree must resolve identically.
	sub := filepath.Join(wt, "deep", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := ProjectFor(sub); got != "mainrepo/wt-feature" {
		t.Fatalf("ProjectFor(worktree subdir) = %q, want %q", got, "mainrepo/wt-feature")
	}
}

// TestProjectForOutsideAGitRepoIsUnchanged pins the fallback. Not every
// directory is a checkout, and the old basename behaviour is the only
// sensible answer there — changing it would be a gratuitous break.
func TestProjectForOutsideAGitRepoIsUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plaindir", "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := ProjectFor(dir); got != "sub" {
		t.Fatalf("ProjectFor(%s) = %q, want the basename %q outside a repo", dir, got, "sub")
	}
	if got := ProjectFor(""); got != "" {
		t.Fatalf("ProjectFor(\"\") = %q, want empty so callers' own fallbacks still fire", got)
	}
}

// TestProjectForIgnoresANonWorktreeGitFile guards against guessing. A
// .git file that is not a worktree pointer (a submodule, for instance)
// must be treated as an ordinary repository — inventing a parent name
// would silently merge two unrelated projects.
func TestProjectForIgnoresANonWorktreeGitFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "submod")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../.git/modules/submod\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	pc := ProjectContextFor(root)
	if pc.IsWorktree {
		t.Fatalf("a submodule .git file was treated as a worktree: %+v", pc)
	}
	if pc.Primary != "submod" {
		t.Fatalf("Primary = %q, want %q", pc.Primary, "submod")
	}
}
