package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dispatchSubcommands reads main.go's own switch, so these guards track
// the real dispatch table rather than a list that could drift alongside
// the thing it is meant to be checking.
func dispatchSubcommands(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z-]+)"`).FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = true
	}
	if len(out) < 10 {
		t.Fatalf("found only %d subcommands in main.go's switch — the extraction is broken, which "+
			"would leave these guards passing forever while checking nothing", len(out))
	}
	return out
}

// TestHooksJSONInvokesRealSubcommands guards the highest-stakes wiring in
// the project, and nothing checked it before.
//
// hooks/hooks.json invokes subcommands BY NAME through the wrapper. Rename
// or remove one and the Go build still succeeds, every test still passes,
// and every hook dies at runtime — silently, because Claude Code swallows
// a failed hook. Measured: an unknown subcommand prints
// "usage: claude-mem-go ..." and exits 2, so SessionStart's `context` hook
// would emit usage text on stdout exactly where Claude Code expects JSON,
// and memory injection would simply stop with nothing anywhere saying so.
func TestHooksJSONInvokesRealSubcommands(t *testing.T) {
	real := dispatchSubcommands(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}

	// The wrapper form every capture hook uses: run-hook.sh <subcommand>.
	wrapped := regexp.MustCompile(`run-hook\.sh"\s+([a-z-]+)`)
	found := 0
	for event, entries := range cfg.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				m := wrapped.FindStringSubmatch(h.Command)
				if m == nil {
					continue // Setup runs a script directly, not a subcommand
				}
				found++
				if !real[m[1]] {
					t.Errorf("hooks.json wires %s to subcommand %q, which main.go does not dispatch.\n"+
						"  Every %s hook would print usage and exit 2 at runtime, and Claude Code "+
						"swallows that — memory would stop silently.", event, m[1], event)
				}
			}
		}
	}
	if found < 5 {
		t.Fatalf("only %d wrapped hook commands found in hooks.json — expected every capture hook; "+
			"the matcher is probably broken", found)
	}
}

// TestUsageListsEverySubcommand covers a staleness found by running the
// binary with a bad argument and reading what it said: the usage message
// advertised three subcommands (`worker|hook|ingest`) out of eighteen,
// omitting `doctor` and `stats` — the two an operator reaches for first
// when something is wrong.
//
// This is the message a user sees at the exact moment they have mistyped
// something, so it being wrong costs more than its size suggests.
func TestUsageListsEverySubcommand(t *testing.T) {
	real := dispatchSubcommands(t)

	out := captureStderr(t, usage)
	for cmd := range real {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage does not mention the %q subcommand, so a user who mistypes will not "+
				"learn it exists:\n%s", cmd, out)
		}
	}
}

// captureStderr mirrors captureStdout, for output that deliberately goes
// to stderr — usage among it, since a hook's stdout is a protocol channel
// and must not carry human-facing text.
//
// Drains the pipe in a goroutine rather than doing one fixed-size Read.
// The first version did the latter, which short-reads once the usage text
// outgrows the buffer — a guard that silently truncates its own input is
// worse than no guard, since it would keep passing while checking less
// and less.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}
