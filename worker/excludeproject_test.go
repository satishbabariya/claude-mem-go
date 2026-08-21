package worker

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// TestProcessSkipsExcludedProjectBeforeAnyRealWork is the regression
// test for a real feature gap this port had until now: no equivalent at
// all of real claude-mem's CLAUDE_MEM_EXCLUDED_PROJECTS opt-out, checked
// at the top of every automatic hook handler there. Uses a bare *Daemon
// with no sessions cache and no store configured at all — exactly like
// TestProcessPanicRecoverySurvives's setup, but here the point is the
// OPPOSITE: an excluded project's event must be skipped before ever
// reaching d.sessions.getOrCreate (which would otherwise panic on this
// intentionally nil sessions cache) or d.st.Insert (also nil) — proving
// the exclusion check happens first, not just that it exists somewhere.
func TestProcessSkipsExcludedProjectBeforeAnyRealWork(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: log.New(&logBuf, "", 0), ExcludedProjects: "excluded-*"}

	payload := []byte(`{"session_id":"s1","cwd":"/tmp/excluded-project","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.process(context.Background(), payload)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return")
	}

	if !strings.Contains(logBuf.String(), "skip: project excluded") {
		t.Errorf("log output = %q, want a project-excluded skip message", logBuf.String())
	}
	// The real proof: if the exclusion check happened AFTER (or not at
	// all), d.sessions.getOrCreate on this intentionally nil sessions
	// cache would panic — recovered by process's own panic backstop, but
	// that recovery would show up as "PANIC recovered" in the log
	// instead of the clean skip message above. Asserting its absence
	// here catches a regression that moves the exclusion check too late,
	// not just one that removes it outright.
	if strings.Contains(logBuf.String(), "PANIC recovered") {
		t.Errorf("log output = %q, want no panic — the exclusion check must happen before d.sessions is ever touched", logBuf.String())
	}
}

// TestProcessDoesNotSkipAnUnmatchedProject is the non-regression
// counterpart: an empty ExcludedProjects (or one that doesn't match this
// cwd) must not accidentally skip everything — confirmed by letting
// process reach the same nil sessions cache and observing the expected
// PANIC-recovered path instead (proving it genuinely tried to do real
// work, the same way TestProcessPanicRecoverySurvives already does
// without ExcludedProjects set at all).
func TestProcessDoesNotSkipAnUnmatchedProject(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: log.New(&logBuf, "", 0), ExcludedProjects: "excluded-*"}

	payload := []byte(`{"session_id":"s1","cwd":"/tmp/normal-project","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.process(context.Background(), payload)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return")
	}

	if strings.Contains(logBuf.String(), "skip: project excluded") {
		t.Errorf("log output = %q, an unmatched project must not be skipped as excluded", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "PANIC recovered") {
		t.Errorf("log output = %q, want it to have proceeded to real work (and hit the intentionally nil sessions cache) — an unmatched project must not be treated as excluded", logBuf.String())
	}
}
