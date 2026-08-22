package cli

import (
	"fmt"
	"os"
	"sync"
)

// DefaultMaxLogBytes bounds a single log file (worker.log, hook.log,
// context.log, stop.log, mcp.log, file-context.log, start.log) before it
// rotates. There was no cap at all before this: every one of these opens
// with O_APPEND and never truncates, and worker.log in particular gets a
// new line on every PostToolUse event for as long as the daemon runs —
// unbounded growth for a process meant to run for months. One prior
// generation is kept (name -> name+".1"), the simplest form of logrotate's
// own "rotate 1" — this is bounded disk use, not a general log-management
// story (no compression, no timestamped generations, no external
// logrotate integration).
const DefaultMaxLogBytes = 5 * 1024 * 1024 // 5MB

// RotatingWriter is an io.Writer that rotates its underlying file once
// writing to it would exceed maxBytes, keeping exactly one prior
// generation. Safe for concurrent use — the worker daemon's process() runs
// one goroutine per accepted connection, all logging through the same
// *log.Logger.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

func NewRotatingWriter(path string, maxBytes int64) (*RotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RotatingWriter{path: path, maxBytes: maxBytes, f: f, size: info.Size()}, nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			// Rotation failing (e.g. a permissions problem, or the
			// directory disappearing) must not stop logging altogether —
			// fall through and keep writing to the current, oversized
			// file rather than losing log output entirely.
			fmt.Fprintln(os.Stderr, "log rotation failed for", w.path, ":", err)
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked closes the current file, moves it to path+".1" (replacing
// any previous ".1" — only one prior generation is kept), and opens a
// fresh file at path. Called with mu held.
func (w *RotatingWriter) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return err
	}

	_ = os.Remove(w.path + ".1")
	renameErr := os.Rename(w.path, w.path+".1")

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	if renameErr != nil {
		// The rename failed, so this reopened (O_APPEND) the same oversized
		// file — seeding size from 0 would count it as empty and defeat the
		// cap until the next process restart. Stat it so the next write
		// re-attempts rotation; if even Stat fails, 0 is the only honest
		// fallback and the rename error below is already being reported.
		if info, serr := f.Stat(); serr == nil {
			w.size = info.Size()
		}
	}
	return renameErr
}
