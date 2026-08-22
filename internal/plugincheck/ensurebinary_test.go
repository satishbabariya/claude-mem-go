package plugincheck

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// scriptPath locates scripts/ensure-binary.sh relative to this package,
// so the test runs the REAL script that ships in the plugin rather than a
// copy that could drift from it.
func scriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "ensure-binary.sh"))
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("ensure-binary.sh not found at %s: %v", p, err)
	}
	return p
}

// fakePluginRoot builds a minimal but REAL Go module laid out the way the
// plugin is, so `go build ./cmd/claude-mem-go` genuinely succeeds. A stub
// that only pretended to build would not exercise the path that matters.
func fakePluginRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "claude-mem-go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("go.mod", "module fakeplugin\n\ngo 1.21\n")
	write(filepath.Join("cmd", "claude-mem-go", "main.go"),
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"fake version\") }\n")
	return root
}

func runScript(t *testing.T, root string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", scriptPath(t))
	cmd.Env = append(os.Environ(), env...)
	if root != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_PLUGIN_ROOT="+root)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run script: %v", err)
	}
	return string(out), code
}

// TestEnsureBinaryBuildsAMissingBinary is the regression test for a real
// install failure. hooks.json resolves every hook to
// "$CLAUDE_PLUGIN_ROOT/claude-mem-go", and that binary is gitignored — it
// is a build artifact, not source — so a plugin installed from a git
// source ships the complete Go source and NO binary. Every hook then
// fails with "No such file or directory" and nothing is captured.
//
// The failure is quiet in the worst way: `doctor` is a subcommand of the
// very binary that is missing, so the tool an operator would reach for to
// diagnose it cannot run either.
func TestEnsureBinaryBuildsAMissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := fakePluginRoot(t)
	bin := filepath.Join(root, "claude-mem-go")
	if _, err := os.Stat(bin); err == nil {
		t.Fatal("fixture already has a binary; the test would prove nothing")
	}

	out, code := runScript(t, root)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("binary still missing after Setup: %v\nOutput:\n%s", err, out)
	}
	got, err := exec.Command(bin).CombinedOutput()
	if err != nil || !strings.Contains(string(got), "fake version") {
		t.Fatalf("built binary does not run: %v / %q", err, got)
	}
}

// TestEnsureBinaryIsANoOpWhenTheBinaryWorks matters because Setup fires
// once per Claude Code launch: rebuilding every time would put a build on
// the startup path forever, which is exactly what running at Setup was
// meant to avoid.
func TestEnsureBinaryIsANoOpWhenTheBinaryWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := fakePluginRoot(t)
	bin := filepath.Join(root, "claude-mem-go")
	if _, code := runScript(t, root); code != 0 {
		t.Fatalf("initial build failed with exit %d", code)
	}
	before, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	out, code := runScript(t, root)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	after, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the binary was rebuilt even though it already worked — Setup would pay a build on every launch")
	}
}

// TestEnsureBinaryRebuildsAnUnrunnableBinary covers the case existence
// alone cannot catch, and the reason the check executes the binary rather
// than stat-ing it: a binary built for the wrong architecture, or a
// truncated copy, stats perfectly and only fails when run.
func TestEnsureBinaryRebuildsAnUnrunnableBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := fakePluginRoot(t)
	bin := filepath.Join(root, "claude-mem-go")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write broken binary: %v", err)
	}

	if out, code := runScript(t, root); code != 0 {
		t.Fatalf("exit %d. Output:\n%s", code, out)
	}
	got, err := exec.Command(bin).CombinedOutput()
	if err != nil || !strings.Contains(string(got), "fake version") {
		t.Fatalf("an unrunnable binary was not replaced: %v / %q", err, got)
	}
}

// TestEnsureBinaryWithoutGoExplainsItself pins the degraded path. Setup
// must not block the session when it cannot help — memory degrading to
// "nothing captured" is bad, but preventing the user from working over it
// would be worse — so it exits 0 and says exactly what to do.
func TestEnsureBinaryWithoutGoExplainsItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := fakePluginRoot(t)

	// An EMPTY directory as the entire PATH, rather than a hand-picked
	// one like /usr/bin:/bin that merely happens to exclude Go. That
	// guess is platform-dependent and was wrong: it passed on macOS,
	// where Go lives in /opt/homebrew/bin, and failed on the CI runner,
	// where Go is reachable from the default PATH — so the test built a
	// binary and then asserted it had not. Controlling the environment
	// beats guessing at one.
	//
	// bash itself still resolves, because the script is invoked as an
	// argument to bash rather than through its shebang, and the branch
	// under test exits before it needs any external command.
	out, code := runScript(t, root, "PATH="+t.TempDir())
	if code != 0 {
		t.Fatalf("exit %d, want 0 — Setup must never block the session. Output:\n%s", code, out)
	}
	if !strings.Contains(out, "Go is not installed") {
		t.Fatalf("no actionable message when Go is unavailable:\n%s", out)
	}
	if !strings.Contains(out, "go build -o claude-mem-go") {
		t.Fatalf("message does not tell the operator how to fix it:\n%s", out)
	}
}

// TestEnsureBinaryWithoutPluginRootDoesNothing guards against the script
// guessing at a location when it is not running as an installed plugin.
func TestEnsureBinaryWithoutPluginRootDoesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	cmd := exec.Command("bash", scriptPath(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exit non-zero with no CLAUDE_PLUGIN_ROOT: %v\n%s", err, out)
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("produced output with no CLAUDE_PLUGIN_ROOT, want silence:\n%s", out)
	}
}

// --- run-hook.sh: the wrapper every hook goes through ---

func runHookScript(t *testing.T, root, arg string) (string, int) {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "run-hook.sh"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cmd := exec.Command("bash", p, arg)
	cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+root, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return string(out), code
}

// TestRunHookExecsTheBinaryWhenPresent is the healthy path, and the one
// that must not regress: every hook goes through this wrapper now, so a
// mistake here breaks capture entirely rather than degrading it.
func TestRunHookExecsTheBinaryWhenPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "claude-mem-go")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"ran: $*\"\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, code := runHookScript(t, root, "context")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "ran: context") {
		t.Fatalf("the wrapper did not exec the binary with its argument: %q", out)
	}
}

// TestRunHookAnnouncesAMissingBinaryToTheSession covers the failure this
// wrapper exists for. A git-sourced install ships no binary (it is
// gitignored), every hook fails with "No such file or directory", and
// Claude Code swallows that — so memory is silently dead forever, and
// `doctor` cannot diagnose it because doctor is a subcommand of the
// missing binary.
//
// SessionStart's `context` hook is the one place a hook can put text in
// front of the user, so the wrapper answers there with a real
// additionalContext payload rather than failing quietly.
func TestRunHookAnnouncesAMissingBinaryToTheSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := t.TempDir() // deliberately empty: no binary

	out, code := runHookScript(t, root, "context")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — a broken install must degrade, not block: %s", code, out)
	}
	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not the JSON Claude Code expects (%v): %q", err, out)
	}
	if payload.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hookEventName = %q, want SessionStart", payload.HookSpecificOutput.HookEventName)
	}
	ctx := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "go build -o claude-mem-go") {
		t.Fatalf("the message does not tell the user how to fix it: %q", ctx)
	}
	if !strings.Contains(ctx, root) {
		t.Fatalf("the message does not name the plugin root, so the fix is not runnable: %q", ctx)
	}
}

// TestRunHookLogsForNonContextHooks pins the other half: the remaining
// hooks have nowhere to speak to the user, so they must at least leave a
// diagnosis somewhere findable instead of vanishing.
func TestRunHookLogsForNonContextHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := t.TempDir()
	home := t.TempDir()

	p, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "run-hook.sh"))
	cmd := exec.Command("bash", p, "hook")
	cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+root, "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exit non-zero, want 0 so a broken install never blocks a tool call: %v\n%s", err, out)
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("a non-context hook wrote to stdout; Claude Code parses that: %q", out)
	}

	logged, err := os.ReadFile(filepath.Join(home, ".claude-mem-go", "missing-binary.log"))
	if err != nil {
		t.Fatalf("no diagnosis was written anywhere: %v", err)
	}
	if !strings.Contains(string(logged), "go build -o claude-mem-go") {
		t.Fatalf("the log does not say how to fix it: %s", logged)
	}
}

// TestRunHookDetachesStopAndStillDeliversThePayload covers a failure that
// only appears in a real headless session, and that no unit test of the
// Go code could have found.
//
// Measured with a probe plugin: a Stop hook that merely slept and then
// wrote a file produced NOTHING after `claude -p` exited — not at 25
// seconds, and not even at 1. Claude Code tears the hook process down
// when the session ends, and a `-p` session ends the moment the answer is
// printed. The session summary needs a settle wait plus a real model
// call, so it never survived: two full soak sessions captured
// observations and produced no summary at all.
//
// The same probe showed backgrounded work DOES outlive the session, so
// `stop` is detached. The catch, and the reason this test checks the
// payload rather than just the timing: the hook's input arrives on
// stdin, which is a pipe that dies with the session. The first attempt
// detached with `</dev/null` and every run logged "FAILED parsing hook
// payload: EOF". Reading stdin in the wrapper while the parent is still
// alive, then replaying it into the child, is what makes detaching safe.
func TestRunHookDetachesStopAndStillDeliversThePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "received.txt")
	// A stand-in for the real binary: slow enough that an un-detached
	// wrapper would still be blocking when we check, and it records the
	// stdin it was given.
	script := "#!/bin/sh\nsleep 2\ncat > " + marker + "\n"
	if err := os.WriteFile(filepath.Join(root, "claude-mem-go"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	p, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "run-hook.sh"))
	cmd := exec.Command("bash", p, "stop")
	cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+root, "HOME="+t.TempDir())
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`)

	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, out)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the wrapper took %s to return; `stop` must detach, not block — blocking is "+
			"precisely what gets killed when a -p session exits", elapsed)
	}

	// The detached child must still finish, and must still have the payload.
	deadline := time.Now().Add(15 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			got = b
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("the detached stop process never ran, or produced nothing — detaching must not mean discarding")
	}
	if !strings.Contains(string(got), `"session_id":"s1"`) {
		t.Fatalf("the detached process received %q, not the hook payload. Redirecting its stdin from "+
			"/dev/null is what caused 'FAILED parsing hook payload: EOF' on every real Stop.", got)
	}
}

// TestRunHookDoesNotDetachOtherHooks pins the scope. PostToolUse forwards
// to the daemon and exits in milliseconds — the long work happens inside
// the worker, which is already detached — so detaching it too would add a
// process for nothing and lose the exec.
func TestRunHookDoesNotDetachOtherHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "claude-mem-go"),
		[]byte("#!/bin/sh\nsleep 1\necho ran-$1\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	p, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "run-hook.sh"))
	cmd := exec.Command("bash", p, "hook")
	cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+root, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ran-hook") {
		t.Fatalf("a non-stop hook did not run in the foreground; its output was lost: %q", out)
	}
}
