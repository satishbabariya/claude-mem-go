package worker

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
)

func nopLogger() *logging.Logger { return logging.New(io.Discard, "", 0) }

func TestStatsCountersSnapshotReflectsIncrements(t *testing.T) {
	var c statsCounters
	c.processed.Add(3)
	c.duplicates.Add(1)
	c.observerErrors.Add(2)
	c.insertErrors.Add(1)
	c.embedErrors.Add(4)
	c.touch()

	s := c.snapshot(5, 1, 2)
	if s.Processed != 3 || s.Duplicates != 1 || s.ObserverErrors != 2 || s.InsertErrors != 1 || s.EmbedErrors != 4 {
		t.Fatalf("snapshot = %+v, want counters to match what was added", s)
	}
	if s.CachedSessions != 5 || s.PoolInFlight != 1 || s.PoolCapacity != 2 {
		t.Fatalf("snapshot = %+v, want the pool/session args passed through", s)
	}
	if s.LastActivityAt == "" {
		t.Fatal("LastActivityAt is empty after touch()")
	}
	if s.UpdatedAt == "" {
		t.Fatal("UpdatedAt is empty")
	}
}

func TestSnapshotOmitsLastActivityBeforeAnyTouch(t *testing.T) {
	var c statsCounters
	s := c.snapshot(0, 0, 0)
	if s.LastActivityAt != "" {
		t.Fatalf("LastActivityAt = %q on a daemon that never processed anything, want empty", s.LastActivityAt)
	}
}

// TestWriteStatsFileThenReadStatsFileRoundTrips is the real property this
// exists for: doctor (a separate process from the worker daemon) reads
// whatever the daemon last wrote, so the write and read sides must agree
// on the exact same shape.
func TestWriteStatsFileThenReadStatsFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker-stats.json")
	want := Stats{
		Processed: 10, Duplicates: 2, ObserverErrors: 1, InsertErrors: 0, EmbedErrors: 3,
		CachedSessions: 4, PoolInFlight: 1, PoolCapacity: 2,
		LastActivityAt: "2026-08-20T12:00:00Z", UpdatedAt: "2026-08-20T12:00:01Z",
	}
	if err := writeStatsFile(path, want); err != nil {
		t.Fatalf("writeStatsFile: %v", err)
	}
	got, err := ReadStatsFile(path)
	if err != nil {
		t.Fatalf("ReadStatsFile: %v", err)
	}
	if got != want {
		t.Fatalf("ReadStatsFile round-trip = %+v, want %+v", got, want)
	}
}

func TestReadStatsFileMissingFileReturnsError(t *testing.T) {
	_, err := ReadStatsFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("ReadStatsFile on a missing file: want an error, got nil")
	}
}

// TestWriteStatsFileOverwritesAtomically confirms a second write replaces
// the first rather than appending or corrupting it — writeStatsFile's
// temp-file-then-rename approach exists specifically for this.
func TestWriteStatsFileOverwritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker-stats.json")
	if err := writeStatsFile(path, Stats{Processed: 1}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeStatsFile(path, Stats{Processed: 2}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := ReadStatsFile(path)
	if err != nil {
		t.Fatalf("ReadStatsFile: %v", err)
	}
	if got.Processed != 2 {
		t.Fatalf("Processed = %d after two writes, want 2 (the second write's value, cleanly)", got.Processed)
	}
}

func TestDaemonStatsReflectsCountersWithoutARunningSessionCache(t *testing.T) {
	d := &Daemon{Log: nopLogger()}
	d.counters.processed.Add(7)
	s := d.Stats()
	if s.Processed != 7 {
		t.Fatalf("Stats().Processed = %d, want 7", s.Processed)
	}
	if s.CachedSessions != 0 || s.PoolInFlight != 0 || s.PoolCapacity != 0 {
		t.Fatalf("Stats() with no sessionCache set = %+v, want all pool/session fields zero, not a panic or garbage", s)
	}
}

func TestRecordStatsIsANoopWithoutAStatsPath(t *testing.T) {
	d := &Daemon{Log: nopLogger()} // StatsPath left empty on purpose
	d.recordStats()                // must not panic or attempt to write anywhere
}
