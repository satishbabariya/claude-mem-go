package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-mem-go/worker"
)

// runDoctor runs cmdDoctor with stdout captured, against a socket that is
// deliberately not listening — the "daemon is dead" case.
func runDoctorNoDaemon(t *testing.T, statsPath, dbPath string) (string, int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var code int
	out := captureStdout(t, func() {
		code = cmdDoctor([]string{
			"-socket", filepath.Join(t.TempDir(), "not-listening.sock"),
			"-stats", statsPath,
			"-db", dbPath,
			"-embed-model", "",
		})
	})
	return out, code
}

func writeDoctorStats(t *testing.T, s worker.Stats) string {
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

// TestDoctorDoesNotReportALiveStateForADeadDaemon covers a false positive
// found by reading doctor's own output as an operator would, rather than
// by any test.
//
// A dead daemon leaves its stats file behind. doctor read that file
// unconditionally, so on a machine with no daemon running it printed
// "worker daemon not running" and then, from the same leftover file, the
// daemon's pool saturation, its build version, and a CRITICAL store
// mismatch — failing the entire run over a process that did not exist.
// Every one of those is a claim about a LIVE daemon.
func TestDoctorDoesNotReportALiveStateForADeadDaemon(t *testing.T) {
	stats := writeDoctorStats(t, worker.Stats{
		Processed:    2,
		PoolInFlight: 2,
		PoolCapacity: 2,
		Version:      "claude-mem-go deadbeef (built 2020-01-01T00:00:00Z, go1.20.0)",
		PID:          4242,
		Store:        "/somewhere/else/observations.db",
		UpdatedAt:    "2026-01-01T00:00:00Z",
	})
	db := filepath.Join(t.TempDir(), "doc.db")

	out, code := runDoctorNoDaemon(t, stats, db)

	for _, phantom := range []string{"observer slot", "DIFFERENT store", "older build"} {
		if strings.Contains(out, phantom) {
			t.Errorf("reported %q for a daemon that is not running:\n%s", phantom, out)
		}
	}
	if code != 0 {
		t.Errorf("exit %d — a dead daemon's leftover stats must not fail the run critically:\n%s", code, out)
	}
	// The history is still worth showing, but must be labelled as history.
	if !strings.Contains(out, "last worker activity before it stopped") {
		t.Errorf("the stats were dropped entirely; they are still useful as history:\n%s", out)
	}
}

// TestDoctorStillShowsActivityLabelledAsHistory pins the wording, because
// the fix would be just as wrong if it presented stale numbers as current.
func TestDoctorStillShowsActivityLabelledAsHistory(t *testing.T) {
	stats := writeDoctorStats(t, worker.Stats{Processed: 7, PoolCapacity: 2})
	out, _ := runDoctorNoDaemon(t, stats, filepath.Join(t.TempDir(), "d.db"))

	if !strings.Contains(out, "processed=7") {
		t.Fatalf("activity numbers were lost:\n%s", out)
	}
	if strings.Contains(out, "… worker activity: processed=7") {
		t.Fatalf("stale numbers are presented as current activity:\n%s", out)
	}
}
