package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRotatingWriterRotatesWhenExceedingMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := NewRotatingWriter(path, 50)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	line := []byte("0123456789\n") // 11 bytes
	for i := 0; i < 4; i++ {       // 44 bytes total — under 50, no rotation yet
		if _, err := w.Write(line); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("path+.1 exists after only 44 bytes written (cap 50) — rotated too early")
	}

	// This write would push the file to 55 bytes, over the 50-byte cap —
	// must rotate before writing it.
	if _, err := w.Write(line); err != nil {
		t.Fatalf("Write (triggers rotation): %v", err)
	}

	oldGen, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated generation: %v", err)
	}
	if bytes.Count(oldGen, []byte("\n")) != 4 {
		t.Fatalf("rotated generation has %d lines, want 4 (everything written before the rotating write)", bytes.Count(oldGen, []byte("\n")))
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current file: %v", err)
	}
	if bytes.Count(current, []byte("\n")) != 1 {
		t.Fatalf("current file has %d lines, want 1 (just the write that triggered rotation)", bytes.Count(current, []byte("\n")))
	}
}

// TestRotatingWriterKeepsOnlyOnePriorGeneration confirms rotating a second
// time replaces path+".1" rather than accumulating .2, .3, etc. — this is
// deliberately a single-generation scheme, not general log management.
func TestRotatingWriterKeepsOnlyOnePriorGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := NewRotatingWriter(path, 20)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	if _, err := w.Write([]byte("generation-one-marker\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Write([]byte("generation-two-marker-that-is-long-enough\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Write([]byte("generation-three-trigger\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	gen1, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read path+.1: %v", err)
	}
	if bytes.Contains(gen1, []byte("generation-one-marker")) {
		t.Fatalf("path+.1 still contains the FIRST generation — a second rotation should have replaced it, not kept both: %q", gen1)
	}
	if !bytes.Contains(gen1, []byte("generation-two-marker")) {
		t.Fatalf("path+.1 = %q, want it to contain the second generation (the one just before the most recent rotation)", gen1)
	}

	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatal("path+.2 exists — this scheme should never accumulate more than one prior generation")
	}
}

func TestRotatingWriterConcurrentWritesAreSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := NewRotatingWriter(path, 200) // small enough to force several rotations
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.Write([]byte("concurrent log line 0123456789\n")); err != nil {
				t.Errorf("Write: %v", err)
			}
		}()
	}
	wg.Wait()
	// No assertion beyond "the race detector and the writes above didn't
	// fail" — the property under test is safety, not exact byte counts
	// (rotation timing under concurrency is inherently racy in which
	// writes land in which generation).
}

func TestNewRotatingWriterReopensAnExistingFileWithoutTruncating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(path, []byte("pre-existing content\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	w, err := NewRotatingWriter(path, DefaultMaxLogBytes)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	if _, err := w.Write([]byte("appended line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(content, []byte("pre-existing content")) {
		t.Fatalf("content = %q, want the pre-existing content preserved (O_APPEND, not truncated)", content)
	}
	if !bytes.Contains(content, []byte("appended line")) {
		t.Fatalf("content = %q, want the newly appended line too", content)
	}
}

// TestRotatingWriterRetriesRotationAfterRenameFails covers a cap-defeating
// bug: when os.Rename failed, rotateLocked reopened the same oversized
// file but reset size to 0, so the writer believed it was empty and did
// not try to rotate again until the process restarted. Blocking the
// rename with a non-empty directory at path+".1" (Remove and Rename both
// fail on it, on macOS and Linux alike) reproduces that, then unblocking
// it proves the next write re-attempts and succeeds.
func TestRotatingWriterRetriesRotationAfterRenameFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	blocker := path + ".1"
	if err := os.MkdirAll(filepath.Join(blocker, "child"), 0o700); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}

	w, err := NewRotatingWriter(path, 50)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	line := []byte("0123456789\n") // 11 bytes
	for i := 0; i < 5; i++ {       // the 5th write (55 bytes) triggers a rotation that cannot rename
		if _, err := w.Write(line); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if got := w.size; got != 55 {
		t.Fatalf("size after a failed rotation = %d, want 55 (the reopened file's real size, not 0)", got)
	}

	// Unblock and write once more: the writer must rotate now, moving all
	// six prior lines aside, rather than believing the file is tiny.
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	if _, err := w.Write(line); err != nil {
		t.Fatalf("Write (should rotate): %v", err)
	}
	oldGen, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotation was not re-attempted after the failed rename: %v", err)
	}
	if n := bytes.Count(oldGen, []byte("\n")); n != 5 {
		t.Fatalf("rotated generation has %d lines, want 5", n)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if n := bytes.Count(current, []byte("\n")); n != 1 {
		t.Fatalf("current file has %d lines, want 1", n)
	}
}
