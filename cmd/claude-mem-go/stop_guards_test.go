package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"
)

// runStopWithPayload runs cmdStop with payload on stdin against an
// isolated HOME, and returns everything the hook logged. HOME is
// redirected because memory.DefaultHome resolves through os.UserHomeDir —
// which keeps the test off the developer's real stop.log, and, more to
// the point, makes the log readable as the assertion surface. cmdStop is
// fire-and-forget by design (nothing reads its stdout, and it returns 0
// on essentially every path), so its log is the only place its decisions
// are observable.
func runStopWithPayload(t *testing.T, dbPath, payload string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	w.Close()

	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	// -embed-model "" keeps Ollama out of it; the guards under test run
	// long before any embedding would happen anyway.
	cmdStop([]string{"-db", dbPath, "-embed-model", ""})

	out, err := os.ReadFile(filepath.Join(home, ".claude-mem-go", "stop.log"))
	if err != nil {
		return ""
	}
	return string(out)
}

func seedSummarizedSession(t *testing.T, sessionID string, withSummary bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stop.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert(context.Background(), sessionID, "guard-proj", "Bash",
		memory.ContentHash(sessionID, "Bash", "did a thing", "1"),
		memory.Observation{Type: "change", Title: "did a thing"}, 0); err != nil {
		t.Fatalf("Insert observation: %v", err)
	}
	if withSummary {
		// The same shape cmdStop itself writes: type "summary", tool name
		// "SessionSummary", and the session-derived content hash.
		if _, err := st.Insert(context.Background(), sessionID, "guard-proj", "SessionSummary",
			memory.ContentHash(sessionID, "SessionSummary", "session-summary", ""),
			memory.Observation{Type: "summary", Title: "already summarized"}, 0); err != nil {
			t.Fatalf("Insert summary: %v", err)
		}
	}
	return path
}

// TestStopSkipsWhenStopHookActive covers the guard Claude Code asks for by
// name: when a Stop hook blocks a turn from ending, the turn is retried
// and every Stop hook fires again with stop_hook_active=true, up to a cap
// that defaults to 8. This hook never blocks, so it never causes that
// retry — but it is dragged along by any other Stop hook that does, and
// being dragged along was measured at 17.3 seconds and one real billed
// observer call per retry, all to produce a summary the content hash
// then rejects.
func TestStopSkipsWhenStopHookActive(t *testing.T) {
	// No summary present, so the ONLY thing that can stop this run is the
	// stop_hook_active guard — if it regresses, the run proceeds to the
	// observer instead of returning here.
	dbPath := seedSummarizedSession(t, "sess-retry", false)

	logOut := runStopWithPayload(t, dbPath, `{"session_id":"sess-retry","cwd":"/tmp/guard-proj",`+
		`"transcript_path":"/tmp/t.jsonl","hook_event_name":"Stop","stop_hook_active":true}`)

	if !strings.Contains(logOut, "stop_hook_active") {
		t.Fatalf("Stop did not skip on stop_hook_active=true. Log:\n%s", logOut)
	}
	if strings.Contains(logOut, "persisted session summary") {
		t.Fatalf("Stop summarized during a blocked-turn retry, want it to return success untouched. Log:\n%s", logOut)
	}
}

// TestStopSkipsAnAlreadySummarizedSession covers the more general guard.
// The final insert was always idempotent — the content hash is derived
// from the session id alone, not the model's output — but that check
// happened last, after the wait budget and after paying for an observer
// call whose result was then discarded.
func TestStopSkipsAnAlreadySummarizedSession(t *testing.T) {
	dbPath := seedSummarizedSession(t, "sess-done", true)

	// stop_hook_active is FALSE here on purpose: this must be caught by
	// the already-summarized check on its own, not by the guard above.
	logOut := runStopWithPayload(t, dbPath, `{"session_id":"sess-done","cwd":"/tmp/guard-proj",`+
		`"transcript_path":"/tmp/t.jsonl","hook_event_name":"Stop","stop_hook_active":false}`)

	if !strings.Contains(logOut, "already summarized") {
		t.Fatalf("Stop did not skip a session that already has its summary. Log:\n%s", logOut)
	}
	if strings.Contains(logOut, "persisted session summary") {
		t.Fatalf("Stop re-summarized an already-summarized session. Log:\n%s", logOut)
	}
}

// TestStopDoesNotSkipAFreshSession is the counterweight, and the reason
// the two tests above mean anything: a guard that fired unconditionally
// would satisfy both of them perfectly while silently disabling session
// summaries entirely.
//
// It asserts on what happens AFTER the guards rather than on a completed
// summary, because completing one needs a real `claude` observer call.
// Reaching the observer at all is the proof that neither guard fired —
// whether that call then succeeds or fails for lack of a CLI is beside
// the point here, and is covered end to end by hand instead (a fresh
// session summarized normally: 17.6s, $0.0043, one summary row).
func TestStopDoesNotSkipAFreshSession(t *testing.T) {
	dbPath := seedSummarizedSession(t, "sess-new", false)

	logOut := runStopWithPayload(t, dbPath, `{"session_id":"sess-new","cwd":"/tmp/guard-proj",`+
		`"transcript_path":"/tmp/t.jsonl","hook_event_name":"Stop","stop_hook_active":false}`)

	if strings.Contains(logOut, "stop_hook_active") || strings.Contains(logOut, "already summarized") {
		t.Fatalf("a fresh session was skipped by one of the redundancy guards — "+
			"that would disable session summaries outright. Log:\n%s", logOut)
	}
}
