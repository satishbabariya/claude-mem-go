package worker

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
)

// TestProcessTreatsAnEmptyPayloadAsALivenessProbe covers a
// misclassification that a clean soak surfaced: three healthy sessions
// produced three ERROR lines in worker.log.
//
// worker.IsRunning dials the socket and closes it immediately — that is
// how `start` decides whether to spawn and how `doctor` reports
// reachability — so a zero-byte connection arrives on every SessionStart
// by design. Logging it as a failure was always wrong, and became
// actively misleading once severities existed, since ERROR is exactly
// what an operator greps for.
func TestProcessTreatsAnEmptyPayloadAsALivenessProbe(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.NewAtLevel(&logBuf, "", 0, logging.Debug)}

	done := make(chan struct{})
	go func() { defer close(done); d.process(context.Background(), []byte{}) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return on an empty payload")
	}

	out := logBuf.String()
	if strings.Contains(out, "ERROR") {
		t.Fatalf("a liveness probe logged at ERROR — this fires on every SessionStart, so it would "+
			"fill the log an operator greps for with false failures:\n%s", out)
	}
	if !strings.Contains(out, "DEBUG") || !strings.Contains(out, "liveness probe") {
		t.Fatalf("the probe left no debug trace; it should still be visible when asked for:\n%s", out)
	}
	// It must return before touching anything: this bare Daemon has no
	// sessions cache and no store, so reaching further would panic.
	if strings.Contains(out, "PANIC") {
		t.Fatalf("process went past the probe check:\n%s", out)
	}
}

// TestProcessStillReportsATruncatedPayload is the counterweight. A client
// that died mid-send leaves a PARTIAL payload, which is a real failure and
// must keep its error severity — the probe check must key on zero bytes
// specifically, not on "unparseable".
func TestProcessStillReportsATruncatedPayload(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.NewAtLevel(&logBuf, "", 0, logging.Debug)}

	done := make(chan struct{})
	go func() { defer close(done); d.process(context.Background(), []byte(`{"session_id":"s1"`)) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return")
	}

	out := logBuf.String()
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "FAILED parsing payload") {
		t.Fatalf("a truncated payload was not reported as an error; that is a real client failure "+
			"and must not be swallowed along with the probes:\n%s", out)
	}
}
