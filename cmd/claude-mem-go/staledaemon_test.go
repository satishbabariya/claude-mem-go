package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"claude-mem-go/worker"
)

func writeStats(t *testing.T, s worker.Stats) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "worker-stats.json")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestStaleDaemonDetectsAnOlderBuild is the regression test for a failure
// found by running the whole loop end to end rather than by any unit
// test. The daemon is the one long-lived process here, so after an
// upgrade it keeps applying the rules it started with while every
// short-lived hook runs the new binary.
//
// Measured: a daemon up for ~28 hours across sixteen commits was still
// using the pre-git-root project naming, so a real session in a
// subdirectory wrote observations under project "auth" (basename) while
// the freshly built SessionStart hook looked them up under "repo" (git
// root). Writes and reads silently disagreed, and the project-naming fix
// was defeated by a process that simply never restarted. `start` only
// ever asked whether a daemon was running, never which one.
func TestStaleDaemonDetectsAnOlderBuild(t *testing.T) {
	p := writeStats(t, worker.Stats{Version: "claude-mem-go deadbeef (built 2020-01-01T00:00:00Z, go1.20.0)", PID: 4242})

	stale, running, pid := staleDaemon(p)
	if !stale {
		t.Fatal("a daemon on a different build was not reported stale")
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want 4242 — without it there is nothing to signal", pid)
	}
	if running == "" {
		t.Fatal("the running build was not reported, so the log could not say what is being replaced")
	}
}

// TestStaleDaemonLeavesAMatchingBuildAlone is the counterweight, and the
// one that matters most: `start` runs on every SessionStart, so a check
// that reported stale too eagerly would restart the daemon at the top of
// every session — far worse than the bug it fixes.
func TestStaleDaemonLeavesAMatchingBuildAlone(t *testing.T) {
	p := writeStats(t, worker.Stats{Version: currentBuildVersion(), PID: os.Getpid()})
	if stale, _, _ := staleDaemon(p); stale {
		t.Fatal("a daemon on the SAME build was reported stale — this would restart the worker on every session")
	}
}

// TestStaleDaemonIsConservativeWhenUnknown pins the fail-safe direction.
// Killing a working daemon on a guess is worse than leaving a possibly-old
// one running, so every unknown must read as "not stale" — including the
// case that is guaranteed to occur in practice: a daemon started before
// the version field existed at all.
func TestStaleDaemonIsConservativeWhenUnknown(t *testing.T) {
	cases := map[string]worker.Stats{
		"no version field (daemon predates it)": {PID: 99},
		"no pid (nothing to signal)":            {Version: "something-old"},
		"empty stats":                           {},
	}
	for name, st := range cases {
		if stale, _, _ := staleDaemon(writeStats(t, st)); stale {
			t.Errorf("%s: reported stale, want conservative false", name)
		}
	}

	if stale, _, _ := staleDaemon(filepath.Join(t.TempDir(), "does-not-exist.json")); stale {
		t.Error("a missing stats file reported stale, want conservative false")
	}

	bad := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if stale, _, _ := staleDaemon(bad); stale {
		t.Error("a corrupt stats file reported stale, want conservative false")
	}
}
