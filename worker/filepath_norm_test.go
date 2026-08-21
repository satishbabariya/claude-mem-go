package worker

import (
	"bytes"
	"claude-mem-go/pool"
	"context"
	"path/filepath"
	"testing"
	"time"

	"claude-mem-go/logging"
	"claude-mem-go/observer"
	"claude-mem-go/store"
	"claude-mem-go/transcript"
)

// relPathHandle is an observer that emits a RELATIVE file path, which is
// exactly what the real model does some of the time: it is told only
// "<file>...</file>", so it sometimes shortens an absolute tool input to
// a repo-relative path.
type relPathHandle struct{}

func (relPathHandle) Observe(tc transcript.ToolCall) (observer.Turn, error) {
	return observer.Turn{Observation: store.Observation{
		Type:      "discovery",
		Title:     "read the token config",
		FilesRead: []string{"src/auth/tokens.go"},
	}}, nil
}
func (relPathHandle) Close() error { return nil }

// TestProcessStoresAbsoluteFilePaths is the CI-level guard for a bug that
// only a full-stack soak surfaced: the PreToolUse file-context feature
// worked by luck.
//
// The hook looks up the path Claude Code puts in the tool payload, which
// is absolute. The worker stored the observer's output verbatim, so a
// relative path never matched and the lookup silently found nothing.
// Measured in this project's own store: 8 absolute vs 7 relative paths
// recorded, and only 2 successful injections across the entire history of
// file-context.log. A two-session soak reproduced it exactly — session
// one recorded "src/auth/tokens.go", session two looked up the absolute
// path and found nothing.
//
// This drives the real d.process against a real store, because the fix
// lives on the write path and a test of NormalizeFilePath alone would not
// notice if the worker stopped calling it.
func TestProcessStoresAbsoluteFilePaths(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "w.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.New(&logBuf, "", 0), st: st}
	d.sessions = newSessionCache(pool.New(2), func(ctx context.Context) (observer.Handle, error) {
		return relPathHandle{}, nil
	})

	cwd := t.TempDir() // a real absolute directory to resolve against
	payload := []byte(`{"session_id":"s1","cwd":"` + cwd + `","hook_event_name":"PostToolUse",` +
		`"tool_name":"Read","tool_input":{"file_path":"src/auth/tokens.go"},"tool_response":{}}`)

	done := make(chan struct{})
	go func() { defer close(done); d.process(context.Background(), payload) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("process did not return")
	}

	want := filepath.Join(cwd, "src/auth/tokens.go")

	// The lookup the hook actually performs.
	got, err := st.ObservationsForFile(filepath.Base(cwd), want, 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("looking up the absolute path %q found %d observations, want 1.\n"+
			"The observer emitted a RELATIVE path; if the worker stores it verbatim the hook can "+
			"never match it, which is exactly how file-context silently found nothing.\nLog: %s",
			want, len(got), logBuf.String())
	}
	if fr := got[0].Observation.FilesRead; len(fr) != 1 || fr[0] != want {
		t.Fatalf("stored FilesRead = %v, want [%s] — storage must be canonical, since both "+
			"backends' file indexes derive from this column", fr, want)
	}
}
