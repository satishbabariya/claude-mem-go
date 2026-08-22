package worker

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
)

// TestProcessPanicRecoverySurvives is the regression test for a real,
// severe gap found this iteration: process runs in its own goroutine
// (handleConn's `go d.process(...)`), and neither it nor anything else in
// this package had panic recovery — an unrecovered panic here doesn't
// just fail one event, it crashes the ENTIRE daemon process, taking
// memory capture down for every project on the machine sharing this one
// daemon. A real, reproducible panic existed in
// sqlite.Store.SemanticSearch's own limit handling before this iteration
// fixed it, which was reachable from here.
//
// Uses a bare *Daemon with no sessions cache initialized (the zero value,
// not a real running daemon) — process's own call to
// d.sessions.getOrCreate naturally panics on the nil receiver, a real Go
// panic, not a simulated one. Proves the recover() backstop itself works,
// independent of any specific bug.
func TestProcessPanicRecoverySurvives(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.New(&logBuf, "", 0)}

	payload := []byte(`{"session_id":"s1","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.process(context.Background(), payload)
	}()

	select {
	case <-done:
		// Reaching this at all (rather than the test binary itself
		// crashing) proves the panic was recovered, not just that this
		// one goroutine returned.
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return — recover() may not be catching the panic")
	}

	if !strings.Contains(logBuf.String(), "PANIC recovered") {
		t.Errorf("log output = %q, want a PANIC recovered line", logBuf.String())
	}
	if d.counters.observerErrors.Load() != 1 {
		t.Errorf("observerErrors = %d, want 1 (the recovered panic should count as an observer error)", d.counters.observerErrors.Load())
	}
}
